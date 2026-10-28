// Command themis-core runs Themis's core: intake, the incident lifecycle and
// the jobs behind it.
//
// The database URL is read from DATABASE_URL, not a flag, so it does not show
// up in process listings. The Kubernetes connection follows the usual rules:
// --kubeconfig, then KUBECONFIG, then the in-cluster service account.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/hzerrad/themis/internal/core"
)

func main() {
	o := core.Options{DatabaseURL: os.Getenv("DATABASE_URL")}
	var kubeContext string
	flag.StringVar(&o.ConfigPath, "config", "/etc/themis/themis.yaml", "path to the Themis configuration file")
	flag.StringVar(&o.HTTPAddr, "http-addr", ":8080", "address of the intake and API server")
	flag.StringVar(&o.GRPCAddr, "grpc-addr", ":9090", "address of the gRPC API for investigators")
	flag.StringVar(&o.ProbeAddr, "probe-addr", ":8081", "address of the /healthz and /readyz probes")
	flag.StringVar(&o.MetricsAddr, "metrics-addr", ":8082", `address of /metrics; "0" disables it`)
	flag.IntVar(&o.Workers, "workers", 10, "jobs to run at once")
	flag.StringVar(&kubeContext, "kube-context", "", "kubeconfig context to use; empty for the current one")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	kube, err := ctrlconfig.GetConfigWithContext(kubeContext)
	if err != nil {
		fail(log, fmt.Errorf("kubernetes config: %w", err))
	}
	o.Kubernetes = kube
	o.Logger = log

	if err := core.Run(ctrl.SetupSignalHandler(), o); err != nil {
		fail(log, err)
	}
}

func fail(log *slog.Logger, err error) {
	log.Error("themis-core stopped", "error", err)
	os.Exit(1)
}
