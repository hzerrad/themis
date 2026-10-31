package approval

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/store"
)

// Results of an approval, or of an attempt the store refused.
const (
	resultApproved       = "approved"
	resultDenied         = "denied"
	resultExpired        = "expired"
	resultDigestMismatch = "digest_mismatch"
	resultReplayed       = "replayed"
)

var approvalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "themis_approvals_total",
	Help: "Approval decisions: approved, denied, expired; and refused attempts: digest_mismatch for a digest other than the one shown, replayed for an action already decided.",
}, []string{"result"})

func init() {
	metrics.Registry.MustRegister(approvalsTotal)
	for _, r := range []string{resultApproved, resultDenied, resultExpired, resultDigestMismatch, resultReplayed} {
		approvalsTotal.WithLabelValues(r)
	}
}

// countRefusal counts an attempt the store refused, which rolled back.
func countRefusal(err error) {
	switch {
	case errors.Is(err, store.ErrDigestMismatch):
		approvalsTotal.WithLabelValues(resultDigestMismatch).Inc()
	case errors.Is(err, store.ErrAlreadyDecided):
		approvalsTotal.WithLabelValues(resultReplayed).Inc()
	}
}
