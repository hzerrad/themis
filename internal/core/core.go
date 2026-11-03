// Package core assembles and runs themis-core.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/hzerrad/themis/internal/approval"
	"github.com/hzerrad/themis/internal/auth"
	"github.com/hzerrad/themis/internal/checks"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/execution"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/grpcserver"
	"github.com/hzerrad/themis/internal/health"
	"github.com/hzerrad/themis/internal/intake"
	"github.com/hzerrad/themis/internal/intake/alertmanager"
	"github.com/hzerrad/themis/internal/investigation"
	"github.com/hzerrad/themis/internal/notify"
	"github.com/hzerrad/themis/internal/people"
	"github.com/hzerrad/themis/internal/reconcile"
	"github.com/hzerrad/themis/internal/remedy"
	"github.com/hzerrad/themis/internal/resources"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
	"github.com/hzerrad/themis/internal/verify"
	"github.com/hzerrad/themis/internal/web"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// Options configure a run of themis-core.
type Options struct {
	ConfigPath  string
	DatabaseURL string
	Kubernetes  *rest.Config

	// MetricsAddr "0" disables metrics. GRPCAddr serves investigators.
	HTTPAddr string
	GRPCAddr string
	// SetupAddr serves Themis to kubectl port-forward, letting whoever comes
	// that way in as admin until an admin signs in for real. It must be a
	// loopback address; empty disables it.
	SetupAddr   string
	ProbeAddr   string
	MetricsAddr string

	// Workers is how many jobs run at once in this process.
	Workers int

	// DatabaseWait is how long to wait for the database at start, so that
	// Themis and its database can start together. Zero means two minutes.
	DatabaseWait time.Duration

	Logger *slog.Logger
}

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 15 * time.Second
	connectTimeout    = 10 * time.Second
	databaseWait      = 2 * time.Minute
	maxConnectBackoff = 5 * time.Second
)

// Run runs themis-core until ctx is done.
func Run(ctx context.Context, o Options) error {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))
	if err := checkSetupAddr(o.SetupAddr); err != nil {
		return err
	}

	shutdownTracing, err := telemetry.SetupTracing(ctx, "core")
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}
	defer func() { _ = telemetry.StopTracing(shutdownTracing) }()

	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(o.Kubernetes)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}
	registry := evidence.NewRegistry(
		evidence.NewRolloutHistory(),
		evidence.NewPodStates(),
		evidence.NewTargetEvents(),
		evidence.NewReferencedConfig(),
		evidence.NewContainerLogsTail(evidence.ClientsetLogs{Clientset: clientset}),
	)
	if err := checkTemplates(cfg, registry); err != nil {
		return err
	}

	wait := o.DatabaseWait
	if wait == 0 {
		wait = databaseWait
	}
	pool, err := connect(ctx, o.DatabaseURL, wait, log)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Migrations take locks, so concurrent replicas are safe.
	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	s, err := store.New(pool)
	if err != nil {
		return err
	}
	states := store.NewStateCollector(ctx, s)
	if err := metrics.Registry.Register(states); err != nil {
		return fmt.Errorf("register state metrics: %w", err)
	}
	defer metrics.Registry.Unregister(states)

	mgr, err := ctrl.NewManager(o.Kubernetes, ctrl.Options{
		Logger:                 logr.FromSlogHandler(log.Handler()),
		Metrics:                metricsserver.Options{BindAddress: o.MetricsAddr},
		HealthProbeBindAddress: o.ProbeAddr,
		LeaderElection:         false,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	// Later code reads this, not cfg, for anything a resource can change.
	var current config.Provider = cfg
	if cfg.Resources != nil {
		live := resources.New(cfg, mgr.GetAPIReader(), mgr.GetClient(), func(p config.Profile) error {
			if err := checkProfileTemplates(p, registry); err != nil {
				return err
			}
			_, err := governance.CompilePolicy(p.EffectivePolicy())
			return err
		}, log)
		if err := live.Refresh(ctx); err != nil {
			return fmt.Errorf("read configuration resources: %w", err)
		}
		if err := live.SetupWatches(mgr); err != nil {
			return fmt.Errorf("watch configuration resources: %w", err)
		}
		current = live
	}

	workers := river.NewWorkers()
	telemetry.AddUnscopedWorker(workers, &intake.ProcessAlertWorker{
		Config: current,
		Store:  s,
		// Uncached: a few gets per alert need no watch permission.
		Reader: mgr.GetAPIReader(),
		Mapper: mgr.GetRESTMapper(),
	})
	telemetry.AddIncidentWorker(workers, &investigation.PrepareWorker{
		Config:   current,
		Store:    s,
		Reader:   mgr.GetAPIReader(),
		Registry: registry,
		Logger:   log,
	})
	govern, err := governance.NewWorker(current, s, mgr.GetAPIReader())
	if err != nil {
		return err
	}
	telemetry.AddIncidentWorker(workers, govern)
	telemetry.AddIncidentWorker(workers, &governance.RestoreWorker{Governor: govern})
	telemetry.AddUnscopedWorker(workers, &approval.ExpireWorker{Store: s})
	telemetry.AddUnscopedWorker(workers, &investigation.CloseUndecidedWorker{Config: current, Store: s})
	executor, closeExecutor, err := dialExecutor(cfg.Executor)
	if err != nil {
		return err
	}
	defer closeExecutor()
	telemetry.AddIncidentWorker(workers, &execution.Worker{Store: s, Executor: executor})
	telemetry.AddIncidentWorker(workers, &verify.Worker{Config: current, Store: s, Reader: mgr.GetAPIReader()})
	telemetry.AddUnscopedWorker(workers, &reconcile.Worker{Store: s, Reader: mgr.GetAPIReader(), Logger: log})
	telemetry.AddIncidentWorker(workers, &notify.Worker{Store: s, Config: cfg.Notifications, Remedy: remedy.Writer{Config: current}})
	if executor == nil {
		log.WarnContext(ctx, "no executor is configured: approved actions are canceled, never executed")
	}

	runner, err := s.NewWorkerRunner(workers, map[string]int{
		river.QueueDefault:  o.Workers,
		investigation.Queue: o.Workers,
	}, []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
			return approval.ExpireArgs{}, nil
		}, &river.PeriodicJobOpts{RunOnStart: true}),
		river.NewPeriodicJob(river.PeriodicInterval(30*time.Second), func() (river.JobArgs, *river.InsertOpts) {
			return reconcile.Args{}, nil
		}, nil),
		river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
			return investigation.CloseUndecidedArgs{}, nil
		}, nil),
	}, log)
	if err != nil {
		return err
	}

	beat := func(component string) *checks.Heartbeat {
		return &checks.Heartbeat{Store: s, Component: component, Logger: log}
	}
	investigators := &grpcserver.Server{
		Store: s, Config: current, Registry: registry, Reader: mgr.GetAPIReader(), Logger: log, Reviewer: grpcserver.KubeTokenReviewer{Clientset: clientset},
		Heartbeat: beat(store.HeartbeatInvestigator),
	}
	rpc := grpc.NewServer(telemetry.GRPCServerTracing("LeaseInvestigation"), grpc.ChainUnaryInterceptor(investigators.Interceptor(cfg.Investigators.BearerTokenFile)))
	investigators.Register(rpc)

	mux := http.NewServeMux()
	api := &approval.API{Store: s, TokenFile: cfg.API.BearerTokenFile, Access: cfg.API.Access, Logger: log, Remedy: remedy.Writer{Config: current}, Suggestions: investigators, Setup: s}
	api.Sessions, api.Accounts = s, s
	checker := &checks.Checker{Config: current, Store: s, Access: checks.KubeAccess{Clientset: clientset}, OIDC: cfg.API.OIDC != nil, NoInvestigator: cfg.Investigators.Disabled}
	if cfg.Executor != nil {
		checker.Executor = cfg.Executor.Address
	}
	api.Checks = checker
	api.Config, api.Intake = current, cfg.Intake
	if cfg.Resources != nil {
		api.Namespace = cfg.Resources.Namespace
	}
	if o := cfg.API.OIDC; o != nil {
		api.OIDC = true
		(&people.SignIn{Config: o.WithDefaults(), Store: s, Logger: log}).Register(mux)
	}
	(&people.Passwords{Store: s, Logger: log}).Register(mux)
	(&people.SignOut{Store: s, Logger: log}).Register(mux)
	api.Register(mux)
	web.Register(mux)
	mux.Handle(alertmanager.Route, &alertmanager.Handler{
		Receiver:  intake.NewReceiver(s),
		TokenFile: cfg.Intake.BearerTokenFile,
		Logger:    log,
		Heartbeat: beat(store.HeartbeatAlertmanager),
		Refused:   beat(store.HeartbeatAlertmanagerRefused),
	})

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("database", health.Database(s)); err != nil {
		return err
	}
	if err := mgr.Add(runner); err != nil {
		return err
	}
	if err := mgr.Add(&server{addr: o.HTTPAddr, handler: otelhttp.NewHandler(mux, "http"), log: log}); err != nil {
		return err
	}
	if o.SetupAddr != "" {
		door := &people.Door{Store: s, Next: mux, Logger: log}
		if err := mgr.Add(&server{addr: o.SetupAddr, handler: otelhttp.NewHandler(door, "setup"), log: log}); err != nil {
			return err
		}
	}
	if err := mgr.Add(&grpcRunnable{addr: o.GRPCAddr, server: rpc}); err != nil {
		return err
	}

	log.InfoContext(ctx, "starting themis-core", "http", o.HTTPAddr, "setup", o.SetupAddr, "grpc", o.GRPCAddr, "probes", o.ProbeAddr, "metrics", o.MetricsAddr)
	return mgr.Start(ctx)
}

// checkSetupAddr refuses a setup address others could reach: whoever comes
// in that way is an admin until setup finishes.
func checkSetupAddr(addr string) error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("setup address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("setup address %q must be a loopback address such as 127.0.0.1:8083, since whoever reaches it is an admin until setup finishes", addr)
	}
	return nil
}

// connect waits up to wait for the database to answer, retrying with backoff,
// since on a fresh install it usually starts after Themis.
func connect(ctx context.Context, url string, wait time.Duration, log *slog.Logger) (*pgxpool.Pool, error) {
	if url == "" {
		return nil, errors.New("no database URL: set DATABASE_URL")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	deadline := time.Now().Add(wait)
	backoff := time.Second
	for {
		pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return pool, nil
		}
		if ctx.Err() != nil || time.Now().Add(backoff).After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("database unreachable after %s: %w", wait, err)
		}
		log.InfoContext(ctx, "waiting for the database", "error", err.Error(), "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxConnectBackoff)
	}
}

type server struct {
	addr    string
	handler http.Handler
	log     *slog.Logger
}

// Start serves until ctx is done, then shuts down gracefully.
func (s *server) Start(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down http: %w", err)
	}
	return nil
}

func (s *server) NeedLeaderElection() bool { return false }

// dialExecutor connects to the executor, or returns nil when none is
// configured.
func dialExecutor(cfg *config.Executor) (execution.Executor, func(), error) {
	if cfg == nil {
		return nil, func() {}, nil
	}
	token, err := auth.ReadToken(cfg.BearerTokenFile)
	if err != nil {
		return nil, nil, fmt.Errorf("executor token: %w", err)
	}
	conn, err := grpc.NewClient(cfg.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(grpcserver.BearerToken(token)),
		grpc.WithChainUnaryInterceptor(telemetry.UnaryClientFields()),
		telemetry.GRPCClientTracing())
	if err != nil {
		return nil, nil, fmt.Errorf("dial executor: %w", err)
	}
	return themisv1.NewExecutorServiceClient(conn), func() { _ = conn.Close() }, nil
}

// checkTemplates refuses profiles naming templates Themis does not have.
func checkTemplates(cfg config.Config, r evidence.Registry) error {
	var errs []error
	for _, p := range cfg.Profiles {
		if err := checkProfileTemplates(p, r); err != nil {
			errs = append(errs, fmt.Errorf("profile %s: %w", p.Name, err))
		}
	}
	return errors.Join(errs...)
}

func checkProfileTemplates(p config.Profile, r evidence.Registry) error {
	var errs []error
	for _, name := range p.Evidence.Allowed {
		if _, ok := r[name]; !ok {
			errs = append(errs, fmt.Errorf("unknown evidence template %q", name))
		}
	}
	return errors.Join(errs...)
}

// grpcRunnable serves gRPC until ctx is done, then stops gracefully.
type grpcRunnable struct {
	addr   string
	server *grpc.Server
}

func (g *grpcRunnable) Start(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", g.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", g.addr, err)
	}
	served := make(chan error, 1)
	go func() { served <- g.server.Serve(ln) }()
	select {
	case err := <-served:
		return fmt.Errorf("serve grpc: %w", err)
	case <-ctx.Done():
	}
	stopped := make(chan struct{})
	go func() { g.server.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(shutdownTimeout):
		g.server.Stop()
	}
	return nil
}

func (g *grpcRunnable) NeedLeaderElection() bool { return false }
