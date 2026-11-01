package core_test

import (
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/evidence"
	// The executor's metrics are served by another binary but share the rules.
	_ "github.com/hzerrad/themis/internal/executor"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// themisMetrics are the metrics the core and executor serve from the start.
// Intake's appear with its first request, and are checked then.
var themisMetrics = []string{
	"themis_abstentions_total",
	"themis_actions_total",
	"themis_approvals_pending",
	"themis_approvals_total",
	"themis_escalations_total",
	"themis_evidence_collected_total",
	"themis_evidence_truncated_total",
	"themis_gitops_refusals_total",
	"themis_incident_transitions_total",
	"themis_incidents",
	"themis_investigations_total",
	"themis_metrics_collection_errors_total",
	"themis_missing_evidence_total",
	"themis_notifications_total",
	"themis_oldest_pending_approval_age_seconds",
	"themis_policy_decisions_total",
	"themis_proposals_total",
	"themis_redactions_total",
	"themis_safety_events_total",
	"themis_stage_duration_seconds",
	"themis_verification_outcomes_total",
}

// Every Themis metric uses allowlisted label names, so a namespace or
// incident ID label cannot slip in, and each label stays within the ceiling.
func TestMetrics_LabelsAreAllowlisted(t *testing.T) {
	// As the core sets them up: its templates, and the database gauges.
	evidence.NewRegistry(evidence.NewRolloutHistory(), evidence.NewPodStates(), evidence.NewTargetEvents(), evidence.NewContainerLogsTail(nil))
	s, _ := pgtest.NewStore(t)
	states := store.NewStateCollector(t.Context(), s)
	metrics.Registry.MustRegister(states)
	t.Cleanup(func() { metrics.Registry.Unregister(states) })

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "themis_") {
			continue
		}
		if !strings.HasPrefix(f.GetName(), "themis_intake_") {
			seen = append(seen, f.GetName())
		}
		values := map[string]map[string]bool{}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if !slices.Contains(telemetry.MetricLabels, l.GetName()) {
					t.Errorf("%s has label %q, which is not allowlisted", f.GetName(), l.GetName())
				}
				if values[l.GetName()] == nil {
					values[l.GetName()] = map[string]bool{}
				}
				values[l.GetName()][l.GetValue()] = true
			}
		}
		for name, vs := range values {
			if len(vs) > telemetry.LabelCeiling {
				t.Errorf("%s{%s} has %d values, over the ceiling of %d", f.GetName(), name, len(vs), telemetry.LabelCeiling)
			}
		}
	}
	if !slices.Equal(seen, themisMetrics) {
		t.Errorf("served %v,\nwant %v: register a new metric with its labels initialized, and list it here and in docs/observability.md", seen, themisMetrics)
	}
}
