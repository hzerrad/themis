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

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/hint"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/resources"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/intake"
	"github.com/hzerrad/themis/internal/incident/intake/alertmanager"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/incident/notification"
	"github.com/hzerrad/themis/internal/incident/reconciliation"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/incident/verification"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/server/grpcapi"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/server/statuspage"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/scenarios"

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
	// Everything Themis can read, check and change. Each part goes to the
	// code that uses it.
	cat, err := catalog.Build(evidence.ClientsetLogs{Clientset: clientset}, scenarios.FS, cfg.Scenarios.Disabled)
	if err != nil {
		return err
	}
	if err := cat.CheckConfig(cfg); err != nil {
		return err
	}
	// The scenario packs' profiles, unless the install has its own of the
	// same name. With resources, those are applied as they are read.
	cfg = cfg.WithProfiles(cat.Profiles)
	if cfg.Resources == nil && len(cfg.Profiles) == 0 {
		return errors.New("no profiles: every scenario pack is disabled and the configuration file has none")
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
		live := resources.New(cfg, mgr.GetAPIReader(), mgr.GetClient(), cat, log)
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
		Evidence: cat.Evidence,
		Checks:   cat.Checks,
		Logger:   log,
	})
	govern := &governance.Worker{Config: current, Store: s, Reader: mgr.GetAPIReader(), Fixes: cat.Fixes}
	telemetry.AddIncidentWorker(workers, govern)
	telemetry.AddIncidentWorker(workers, &governance.RestoreWorker{Governor: govern})
	telemetry.AddUnscopedWorker(workers, &governance.ExpireWorker{Store: s})
	telemetry.AddUnscopedWorker(workers, &investigation.CloseUndecidedWorker{Config: current, Store: s})
	executor, closeExecutor, err := dialExecutor(cfg.Executor)
	if err != nil {
		return err
	}
	defer closeExecutor()
	telemetry.AddIncidentWorker(workers, &governance.ExecuteWorker{Store: s, Executor: executor})
	telemetry.AddIncidentWorker(workers, &verification.Worker{Config: current, Store: s, Reader: mgr.GetAPIReader(), Fixes: cat.Fixes})
	telemetry.AddUnscopedWorker(workers, &reconciliation.Worker{Store: s, Reader: mgr.GetAPIReader(), Logger: log})
	telemetry.AddIncidentWorker(workers, &notification.Worker{Store: s, Config: cfg.Notifications, Remedy: hint.Writer{Config: current}})
	if executor == nil {
		log.WarnContext(ctx, "no executor is configured: approved actions are canceled, never executed")
	}

	runner, err := s.NewWorkerRunner(workers, map[string]int{
		river.QueueDefault:  o.Workers,
		investigation.Queue: o.Workers,
	}, []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
			return governance.ExpireArgs{}, nil
		}, &river.PeriodicJobOpts{RunOnStart: true}),
		river.NewPeriodicJob(river.PeriodicInterval(30*time.Second), func() (river.JobArgs, *river.InsertOpts) {
			return reconciliation.Args{}, nil
		}, nil),
		river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
			return investigation.CloseUndecidedArgs{}, nil
		}, nil),
	}, log)
	if err != nil {
		return err
	}

	beat := func(component string) *statuspage.Heartbeat {
		return &statuspage.Heartbeat{Store: s, Component: component, Logger: log}
	}
	suggestions := &suggestion.Inbox{Store: s, Config: current, Fixes: cat.Fixes, Checks: cat.Checks, Logger: log}
	investigators := &grpcapi.Server{
		Store: s, Config: current, Evidence: cat.Evidence, Checks: cat.Checks, Fixes: cat.Fixes, RootCauses: cat.RootCauses, Reader: mgr.GetAPIReader(), Logger: log, Reviewer: grpcapi.KubeTokenReviewer{Clientset: clientset},
		Heartbeat: beat(store.HeartbeatInvestigator), Suggestions: suggestions,
	}
	rpc := grpc.NewServer(telemetry.GRPCServerTracing("LeaseInvestigation"), grpc.ChainUnaryInterceptor(investigators.Interceptor(cfg.Investigators.BearerTokenFile)))
	investigators.Register(rpc)

	mux := http.NewServeMux()
	api := &httpapi.API{Store: s, TokenFile: cfg.API.BearerTokenFile, Access: cfg.API.Access, Logger: log, Remedy: hint.Writer{Config: current}, Suggestions: suggestions, Fixes: cat.Fixes, CheckList: cat.Checks, Setup: s}
	api.Sessions, api.Accounts = s, s
	checker := &statuspage.Checker{Config: current, Store: s, Access: statuspage.KubeAccess{Clientset: clientset}, OIDC: cfg.API.OIDC != nil, NoInvestigator: cfg.Investigators.Disabled}
	if cfg.Executor != nil {
		checker.Executor = cfg.Executor.Address
	}
	api.Checks = checker
	if cfg.Settings != nil {
		modelSettings := &settings.Settings{Store: s, KeyFile: cfg.Settings.KeyFile, Install: cfg.Model}
		api.Settings, investigators.Settings = modelSettings, modelSettings
		if !cfg.Investigators.Disabled {
			checker.Model = modelSettings
		}
	}
	api.Config, api.Intake = current, cfg.Intake
	if cfg.Resources != nil {
		api.Namespace = cfg.Resources.Namespace
	}
	if o := cfg.API.OIDC; o != nil {
		api.OIDC = true
		(&identity.SignIn{Config: o.WithDefaults(), Store: s, Logger: log}).Register(mux)
	}
	(&identity.Passwords{Store: s, Logger: log}).Register(mux)
	(&identity.SignOut{Store: s, Logger: log}).Register(mux)
	api.Register(mux)
	httpapi.RegisterUI(mux)
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
	if err := mgr.AddReadyzCheck("database", databaseReady(s)); err != nil {
		return err
	}
	if err := mgr.Add(runner); err != nil {
		return err
	}
	if err := mgr.Add(&server{addr: o.HTTPAddr, handler: otelhttp.NewHandler(mux, "http"), log: log}); err != nil {
		return err
	}
	if o.SetupAddr != "" {
		door := &identity.Door{Store: s, Next: mux, Logger: log}
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
func dialExecutor(cfg *config.Executor) (governance.Executor, func(), error) {
	if cfg == nil {
		return nil, func() {}, nil
	}
	secret, err := token.Read(cfg.BearerTokenFile)
	if err != nil {
		return nil, nil, fmt.Errorf("executor token: %w", err)
	}
	conn, err := grpc.NewClient(cfg.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(token.GRPCCredentials(secret)),
		grpc.WithChainUnaryInterceptor(telemetry.UnaryClientFields()),
		telemetry.GRPCClientTracing())
	if err != nil {
		return nil, nil, fmt.Errorf("dial executor: %w", err)
	}
	return themisv1.NewExecutorServiceClient(conn), func() { _ = conn.Close() }, nil
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
