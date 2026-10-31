package store

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/incident"
)

// collectTimeout bounds the query of one scrape.
const collectTimeout = 5 * time.Second

var (
	incidentsDesc = prometheus.NewDesc("themis_incidents",
		"Incidents in each state now, from the database. Every replica reports the same value: aggregate with max.", []string{"state"}, nil)
	approvalsPendingDesc = prometheus.NewDesc("themis_approvals_pending",
		"Actions awaiting a human's approval now, from the database. Aggregate with max.", nil, nil)
	oldestApprovalDesc = prometheus.NewDesc("themis_oldest_pending_approval_age_seconds",
		"Age of the oldest action awaiting approval, or 0 when none waits, from the database. Aggregate with max.", nil, nil)

	collectionErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "themis_metrics_collection_errors_total",
		Help: "Scrapes whose database query failed. Those scrapes report no incident or approval gauges.",
	})
)

func init() {
	metrics.Registry.MustRegister(collectionErrors)
}

// StateCollector reports, at each scrape and with one query, the incidents in
// each state and the actions awaiting approval.
type StateCollector struct {
	ctx   context.Context
	store *Store
}

// NewStateCollector returns a collector whose queries run under ctx.
func NewStateCollector(ctx context.Context, s *Store) *StateCollector {
	return &StateCollector{ctx: ctx, store: s}
}

func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- incidentsDesc
	ch <- approvalsPendingDesc
	ch <- oldestApprovalDesc
}

func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(c.ctx, collectTimeout)
	defer cancel()
	var (
		states  map[string]float64
		pending float64
		oldest  float64
	)
	err := c.store.pool.QueryRow(ctx, `
	SELECT
		(SELECT coalesce(jsonb_object_agg(state, n), '{}') FROM (SELECT state, count(*) AS n FROM incidents GROUP BY state) s),
		count(*)::float8,
		coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8
	FROM actions WHERE state = 'awaiting_approval'
`).Scan(&states, &pending, &oldest)
	if err != nil {
		collectionErrors.Inc()
		return
	}
	for _, s := range incident.States() {
		ch <- prometheus.MustNewConstMetric(incidentsDesc, prometheus.GaugeValue, states[string(s)], string(s))
	}
	ch <- prometheus.MustNewConstMetric(approvalsPendingDesc, prometheus.GaugeValue, pending)
	ch <- prometheus.MustNewConstMetric(oldestApprovalDesc, prometheus.GaugeValue, oldest)
}
