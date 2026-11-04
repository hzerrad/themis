package suggestion

import (
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

var (
	proposalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_proposals_total",
		Help: "Proposals stored, by result: accepted or rejected.",
	}, []string{"result"})

	externalProposalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_external_proposals_total",
		Help: "Proposals stored from outside Themis, by result: accepted or rejected.",
	}, []string{"result"})

	safetyEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_safety_events_total",
		Help: "Proposals rejected for naming another target (wrong_target), a capability the profile does not allow (unknown_capability) or evidence that was never collected (unknown_evidence).",
	}, []string{"kind"})

	requestsLimitedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_requests_limited_total",
		Help: "Requests from outside Themis refused for coming too often, by kind: address, reads or suggestions.",
	}, []string{"kind"})
)

// safetyKinds are the rejections that are safety events.
var safetyKinds = map[themisv1.RejectionReason]string{
	themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH:        "wrong_target",
	themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED: "unknown_capability",
	themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE:       "unknown_evidence",
}

func init() {
	metrics.Registry.MustRegister(proposalsTotal, externalProposalsTotal, safetyEventsTotal, requestsLimitedTotal)
	for _, r := range []string{"accepted", "rejected"} {
		proposalsTotal.WithLabelValues(r)
		externalProposalsTotal.WithLabelValues(r)
	}
	for _, k := range safetyKinds {
		safetyEventsTotal.WithLabelValues(k)
	}
	for _, k := range []string{"address", "reads", "suggestions"} {
		requestsLimitedTotal.WithLabelValues(k)
	}
}

// CountLimited counts a request from outside Themis refused for coming too
// often. kind is address, reads or suggestions: the gRPC API limits the first
// two, suggestions are limited here.
func CountLimited(kind string) {
	requestsLimitedTotal.WithLabelValues(kind).Inc()
}

func result(r *rejection) string {
	if r == nil {
		return "accepted"
	}
	return "rejected"
}

// count counts a stored suggestion, and a rejection that is a safety event,
// once tx commits.
func count(tx pgx.Tx, r *rejection) {
	store.AfterCommit(tx, proposalsTotal.WithLabelValues(result(r)).Inc)
	if r == nil {
		return
	}
	if kind, ok := safetyKinds[r.reason]; ok {
		store.AfterCommit(tx, safetyEventsTotal.WithLabelValues(kind).Inc)
	}
}
