package store

import (
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/evidence"
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
	missingEvidenceTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_missing_evidence_total",
		Help: "Evidence an investigation needed and couldn't have, by template and why: forbidden, not_in_profile, expired, not_found, budget_spent, data_ceiling, timeout, unavailable, or absent when there was nothing.",
	}, []string{"template", "cause"})

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
	metrics.Registry.MustRegister(missingEvidenceTotal, investigationsTotal, transitionsTotal, escalationsTotal, stageDuration)
	for _, t := range []string{evidence.RolloutHistory, evidence.PodStates, evidence.TargetEvents, evidence.ContainerLogsTail} {
		for _, c := range evidence.GapCauses() {
			missingEvidenceTotal.WithLabelValues(t, string(c))
		}
	}
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
