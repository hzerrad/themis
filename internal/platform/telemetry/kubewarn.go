package telemetry

import (
	"context"
	"log/slog"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	kubeWarnings = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "themis_kube_api_deprecation_warnings_total",
		Help: "Warnings the Kubernetes API server returned, such as use of a deprecated API. Each distinct warning is also logged once.",
	})
	registerKubeWarnings sync.Once
)

// maxWarningsLogged bounds the warnings remembered as already logged. Beyond
// it, new warnings are still counted but no longer logged.
const maxWarningsLogged = 256

// ReportKubeWarnings makes every client built from cfg count the warnings the
// Kubernetes API server sends with a response, such as use of a deprecated
// API, and log each distinct one once, so a Kubernetes upgrade that will
// break Themis shows up in its logs and metrics before it does. The counter
// is served by the binaries that call it, from then on.
func ReportKubeWarnings(cfg *rest.Config, log *slog.Logger) {
	registerKubeWarnings.Do(func() { metrics.Registry.MustRegister(kubeWarnings) })
	cfg.WarningHandlerWithContext = &warningHandler{log: log, logged: map[string]struct{}{}}
}

type warningHandler struct {
	log    *slog.Logger
	mu     sync.Mutex
	logged map[string]struct{}
}

// HandleWarningHeaderWithContext handles one Warning header. Code 299 is the
// only one the API server sends (RFC 7234's "miscellaneous persistent warning").
func (h *warningHandler) HandleWarningHeaderWithContext(ctx context.Context, code int, agent, text string) {
	if code != 299 || text == "" {
		return
	}
	kubeWarnings.Inc()
	if h.firstTime(text) {
		h.log.WarnContext(ctx, "kubernetes API warning", "warning", text, "agent", agent)
	}
}

func (h *warningHandler) firstTime(text string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.logged[text]; ok || len(h.logged) >= maxWarningsLogged {
		return false
	}
	h.logged[text] = struct{}{}
	return true
}
