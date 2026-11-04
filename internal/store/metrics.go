package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/incident"
)

// Stages of an incident whose duration is observed.
const (
	stageToProposal   = "to_proposal"
	stageApprovalWait = "approval_wait"
	stageExecution    = "execution"
	stageVerification = "verification"
	stageToClose      = "to_close"
)

// stageOf names the stages that are one state.
var stageOf = map[incident.State]string{
	incident.AwaitingApproval: stageApprovalWait,
	incident.Executing:        stageExecution,
	incident.Verifying:        stageVerification,
}

// How an investigation's lease ended.
const (
	InvestigationProposed        = "proposed"
	InvestigationAbstained       = "abstained"
	InvestigationFailed          = "failed"
	InvestigationLeaseLost       = "lease_lost"
	InvestigationBudgetExhausted = "budget_exhausted"
)

var (
	investigationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_investigations_total",
		Help: "Investigation leases ended, by outcome: proposed, abstained (inconclusive), failed, budget_exhausted, or lease_lost when the lease expired.",
	}, []string{"outcome"})

	transitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_incident_transitions_total",
		Help: "Committed incident state changes, by state left and state entered.",
	}, []string{"from", "to"})

	escalationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_escalations_total",
		Help: "Incidents handed to humans, by the state they were escalated from.",
	}, []string{"from"})

	stageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "themis_stage_duration_seconds",
		Help: "Time an incident spent in a stage: from creation to a proposal (to_proposal) or to its end (to_close), and in approval_wait, execution and verification.",
		// From seconds to the four hours verification may take.
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400},
	}, []string{"stage"})
)

func init() {
	metrics.Registry.MustRegister(investigationsTotal, transitionsTotal, escalationsTotal, stageDuration)
	for _, o := range []string{InvestigationProposed, InvestigationAbstained, InvestigationFailed, InvestigationLeaseLost, InvestigationBudgetExhausted} {
		investigationsTotal.WithLabelValues(o)
	}
	for _, from := range incident.States() {
		for _, to := range incident.States() {
			if incident.CanTransition(from, to) {
				transitionsTotal.WithLabelValues(string(from), string(to))
			}
		}
		if incident.CanTransition(from, incident.Escalated) || incident.CanTransition(from, incident.ContainedEscalated) {
			escalationsTotal.WithLabelValues(string(from))
		}
	}
	for _, s := range []string{stageToProposal, stageApprovalWait, stageExecution, stageVerification, stageToClose} {
		stageDuration.WithLabelValues(s)
	}
}

// CountInvestigation counts a lease that ended with outcome, once tx commits.
func CountInvestigation(tx pgx.Tx, outcome string) {
	AfterCommit(tx, investigationsTotal.WithLabelValues(outcome).Inc)
}

// observeTransition records a committed transition, which spent sinceChange
// seconds in its previous state and sinceCreated since the incident began.
func observeTransition(from, to incident.State, sinceChange, sinceCreated float64) {
	transitionsTotal.WithLabelValues(string(from), string(to)).Inc()
	if to == incident.Escalated || to == incident.ContainedEscalated {
		escalationsTotal.WithLabelValues(string(from)).Inc()
	}
	if to == incident.Proposed {
		stageDuration.WithLabelValues(stageToProposal).Observe(sinceCreated)
	}
	if stage, ok := stageOf[from]; ok {
		stageDuration.WithLabelValues(stage).Observe(sinceChange)
	}
	if to.Terminal() {
		stageDuration.WithLabelValues(stageToClose).Observe(sinceCreated)
	}
}

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
