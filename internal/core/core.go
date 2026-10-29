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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/hzerrad/themis/internal/approval"
	"github.com/hzerrad/themis/internal/auth"
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
	"github.com/hzerrad/themis/internal/reconcile"
	"github.com/hzerrad/themis/internal/store"
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
	HTTPAddr    string
	GRPCAddr    string
	ProbeAddr   string
	MetricsAddr string

	// Workers is how many jobs run at once in this process.
	Workers int

	Logger *slog.Logger
}

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 15 * time.Second
	connectTimeout    = 10 * time.Second
)

// Run runs themis-core until ctx is done.
func Run(ctx context.Context, o Options) error {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

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
		evidence.NewContainerLogsTail(evidence.ClientsetLogs{Clientset: clientset}),
	)
	if err := checkTemplates(cfg, registry); err != nil {
		return err
	}

	pool, err := connect(ctx, o.DatabaseURL)
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

	mgr, err := ctrl.NewManager(o.Kubernetes, ctrl.Options{
		Logger:                 logr.FromSlogHandler(log.Handler()),
		Metrics:                metricsserver.Options{BindAddress: o.MetricsAddr},
		HealthProbeBindAddress: o.ProbeAddr,
		LeaderElection:         false,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, &intake.ProcessAlertWorker{
		Config: cfg,
		Store:  s,
		// Uncached: a few gets per alert need no watch permission.
		Reader: mgr.GetAPIReader(),
		Mapper: mgr.GetRESTMapper(),
	})
	river.AddWorker(workers, &investigation.PrepareWorker{
		Config:   cfg,
		Store:    s,
		Reader:   mgr.GetAPIReader(),
		Registry: registry,
		Logger:   log,
	})
	govern, err := governance.NewWorker(cfg, s, mgr.GetAPIReader())
	if err != nil {
		return err
	}
	river.AddWorker(workers, govern)
	river.AddWorker(workers, &approval.ExpireWorker{Store: s})
	executor, closeExecutor, err := dialExecutor(cfg.Executor)
	if err != nil {
		return err
	}
	defer closeExecutor()
	river.AddWorker(workers, &execution.Worker{Store: s, Executor: executor})
	river.AddWorker(workers, &verify.Worker{Config: cfg, Store: s, Reader: mgr.GetAPIReader()})
	river.AddWorker(workers, &reconcile.Worker{Store: s, Reader: mgr.GetAPIReader(), Logger: log})
	river.AddWorker(workers, &notify.Worker{Store: s, Config: cfg.Notifications})
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
	}, log)
	if err != nil {
		return err
	}

	rpc := grpc.NewServer(grpc.ChainUnaryInterceptor(grpcserver.RequireToken(cfg.Investigators.BearerTokenFile)))
	(&grpcserver.Server{Store: s, Config: cfg, Registry: registry, Reader: mgr.GetAPIReader(), Logger: log}).Register(rpc)

	mux := http.NewServeMux()
	(&approval.API{Store: s, TokenFile: cfg.API.BearerTokenFile, Logger: log}).Register(mux)
	web.Register(mux)
	mux.Handle(alertmanager.Route, &alertmanager.Handler{
		Receiver:  intake.NewReceiver(s),
		TokenFile: cfg.Intake.BearerTokenFile,
		Logger:    log,
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
	if err := mgr.Add(&server{addr: o.HTTPAddr, handler: mux, log: log}); err != nil {
		return err
	}
	if err := mgr.Add(&grpcRunnable{addr: o.GRPCAddr, server: rpc}); err != nil {
		return err
	}

	log.InfoContext(ctx, "starting themis-core", "http", o.HTTPAddr, "grpc", o.GRPCAddr, "probes", o.ProbeAddr, "metrics", o.MetricsAddr)
	return mgr.Start(ctx)
}

func connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	if url == "" {
		return nil, errors.New("no database URL: set DATABASE_URL")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database unreachable: %w", err)
	}
	return pool, nil
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
		grpc.WithPerRPCCredentials(grpcserver.BearerToken(token)))
	if err != nil {
		return nil, nil, fmt.Errorf("dial executor: %w", err)
	}
	return themisv1.NewExecutorServiceClient(conn), func() { _ = conn.Close() }, nil
}

// checkTemplates refuses profiles naming templates Themis does not have.
func checkTemplates(cfg config.Config, r evidence.Registry) error {
	var errs []error
	for _, p := range cfg.Profiles {
		for _, name := range p.Evidence.Allowed {
			if _, ok := r[name]; !ok {
				errs = append(errs, fmt.Errorf("profile %s: unknown evidence template %q", p.Name, name))
			}
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
