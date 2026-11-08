// Command themis-executor executes approved actions. It is the only Themis
// component with write access to workloads; it takes an action ID from the
// core and trusts nothing else in the request. --namespaces and --fixes say
// what it may change, whatever was approved.
//
// The database URL is read from DATABASE_URL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/executor"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/store"
)

func main() {
	var grpcAddr, probeAddr, metricsAddr, tokenFile, kubeContext string
	var level slog.Level
	var scope executor.Scope
	flag.Func("namespaces", "namespaces the executor may change, comma-separated; required", list(&scope.Namespaces))
	flag.Func("fixes", "fixes the executor may apply, comma-separated; empty for every fix", list(&scope.Fixes))
	flag.StringVar(&grpcAddr, "grpc-addr", ":9091", "address of the ExecutorService")
	flag.StringVar(&probeAddr, "probe-addr", ":8081", "address of /healthz and /readyz")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8082", `address of /metrics; "0" disables it`)
	flag.StringVar(&tokenFile, "token-file", "/var/run/secrets/themis/executor-token", "file holding the token the core must send")
	flag.StringVar(&kubeContext, "kube-context", "", "kubeconfig context to use; empty for the current one")
	flag.TextVar(&level, "log-level", slog.LevelInfo, "least severe level logged: debug, info, warn or error")
	flag.Parse()

	log := telemetry.NewLogger(os.Stdout, "executor", level)
	slog.SetDefault(log)
	if err := run(ctrl.SetupSignalHandler(), log, scope, grpcAddr, probeAddr, metricsAddr, tokenFile, kubeContext); err != nil {
		log.Error("themis-executor stopped", telemetry.FieldError, err)
		os.Exit(1)
	}
}

// list sets *to from a comma-separated flag value, leaving out empty names.
func list(to *[]string) func(string) error {
	return func(v string) error {
		*to = nil
		for name := range strings.SplitSeq(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				*to = append(*to, name)
			}
		}
		return nil
	}
}

func run(ctx context.Context, log *slog.Logger, scope executor.Scope, grpcAddr, probeAddr, metricsAddr, tokenFile, kubeContext string) error {
	if err := scope.Check(catalog.Fixes()); err != nil {
		return err
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("no database URL: set DATABASE_URL")
	}
	shutdownTracing, err := telemetry.SetupTracing(ctx, "executor")
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}
	defer func() { _ = telemetry.StopTracing(shutdownTracing) }()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	s, err := store.New(pool)
	if err != nil {
		return err
	}
	kube, err := ctrlconfig.GetConfigWithContext(kubeContext)
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	telemetry.ReportKubeWarnings(kube, log)
	c, err := client.New(kube, client.Options{})
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	srv := grpc.NewServer(telemetry.GRPCServerTracing(), grpc.ChainUnaryInterceptor(token.RequireGRPC(tokenFile), telemetry.UnaryServerFields()))
	(&executor.Server{Executor: newExecutor(s, c, scope, log)}).Register(srv)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", grpcAddr, err)
	}

	probes := &http.Server{Addr: probeAddr, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := s.Ping(pingCtx); err != nil {
				http.Error(w, "database unreachable", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { _ = probes.ListenAndServe() }()

	var metricsSrv *http.Server
	if metricsAddr != "0" {
		metricsSrv = &http.Server{Addr: metricsAddr, ReadHeaderTimeout: 5 * time.Second, Handler: metricsHandler()}
		go func() { _ = metricsSrv.ListenAndServe() }()
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.InfoContext(ctx, "starting themis-executor", "grpc", grpcAddr, "probes", probeAddr, "metrics", metricsAddr)
	select {
	case err := <-served:
		return fmt.Errorf("serve grpc: %w", err)
	case <-ctx.Done():
	}
	srv.GracefulStop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	return probes.Shutdown(shutdownCtx)
}

// newExecutor carries out the fixes Themis ships with, within scope.
func newExecutor(s *store.Store, c client.Client, scope executor.Scope, log *slog.Logger) *executor.Executor {
	return executor.New(s, c, catalog.Fixes(), scope, log)
}

// metricsHandler serves the executor's metrics on /metrics.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	return mux
}
