// Package kubewarn reports the warnings the Kubernetes API server sends with a
// response, such as use of a deprecated API, so a Kubernetes upgrade that will
// break Themis shows up in its logs and metrics before it does.
package kubewarn

import (
	"context"
	"log/slog"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var warnings = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "themis_kube_api_deprecation_warnings_total",
	Help: "Warnings the Kubernetes API server returned, such as use of a deprecated API. Each distinct warning is also logged once.",
})

func init() {
	metrics.Registry.MustRegister(warnings)
}

// maxLogged bounds the warnings remembered as already logged. Beyond it, new
// warnings are still counted but no longer logged.
const maxLogged = 256

// Install makes every client built from cfg count the API server's warnings
// and log each distinct one once.
func Install(cfg *rest.Config, log *slog.Logger) {
	cfg.WarningHandlerWithContext = &handler{log: log, logged: map[string]struct{}{}}
}

type handler struct {
	log    *slog.Logger
	mu     sync.Mutex
	logged map[string]struct{}
}

// HandleWarningHeaderWithContext handles one Warning header. Code 299 is the
// only one the API server sends (RFC 7234's "miscellaneous persistent warning").
func (h *handler) HandleWarningHeaderWithContext(ctx context.Context, code int, agent, text string) {
	if code != 299 || text == "" {
		return
	}
	warnings.Inc()
	if h.firstTime(text) {
		h.log.WarnContext(ctx, "kubernetes API warning", "warning", text, "agent", agent)
	}
}

func (h *handler) firstTime(text string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.logged[text]; ok || len(h.logged) >= maxLogged {
		return false
	}
	h.logged[text] = struct{}{}
	return true
}
