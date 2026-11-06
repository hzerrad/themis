package hint_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/hint"
	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

var (
	writer = hint.Writer{Config: config.Config{
		EnrolledNamespaces: []string{"shop"},
		Profiles:           []config.Profile{{Name: "rollout-regression", Investigation: config.Investigation{MaxEvidenceRequests: 2}}},
	}}
	checkout = store.Incident{Profile: "rollout-regression", Target: incident.TargetRef{Namespace: "shop", Name: "checkout"}}
)

func message(cause evidence.GapCause, template string) string {
	return writer.Message(checkout, store.EvidenceGap{Template: template, Cause: cause, Detail: "connection refused"})
}

var templates = []string{evidence.RolloutHistory, evidence.PodStates, evidence.TargetEvents, evidence.ContainerLogsTail, evidence.ReferencedConfig, evidence.ContainerMemory, evidence.ContainerCPU}

// internalWords are Themis's own terms: what an operator reads never uses them.
var internalWords = []string{
	"gap", "blind", "ambiguous", "abstain", "predicate", "data ceiling", "lease",
	"capability", "digest", "governance", "hypothes", "proposal",
}

func TestMessages_UsePlainWords(t *testing.T) {
	for _, c := range evidence.GapCauses() {
		for _, tmpl := range templates {
			m := message(c, tmpl)
			for _, w := range internalWords {
				if regexp.MustCompile(`\b` + w).MatchString(strings.ToLower(m)) {
					t.Errorf("%s, %s: %q uses the internal word %q", c, tmpl, m, w)
				}
			}
			if !strings.HasSuffix(m, ".") {
				t.Errorf("%s, %s: %q is not a sentence", c, tmpl, m)
			}
		}
	}
	for _, h := range []string{hint.Headline(true), hint.Headline(false)} {
		for _, w := range internalWords {
			if regexp.MustCompile(`\b` + w).MatchString(strings.ToLower(h)) {
				t.Errorf("headline %q uses %q", h, w)
			}
		}
	}
}

// Each message tells the operator what to change, in their own settings.
func TestRemedy(t *testing.T) {
	away := checkout
	away.Target.Namespace = "billing"
	tests := []struct {
		name     string
		incident store.Incident
		cause    evidence.GapCause
		template string
		want     string
	}{
		{"forbidden, not enrolled", away, evidence.GapForbidden, evidence.RolloutHistory, "Rollout history: access denied in billing, which isn't enrolled. Add billing to enrolledNamespaces."},
		{"forbidden, enrolled", checkout, evidence.GapForbidden, evidence.TargetEvents, "Events: access denied in shop. Grant read access in the Role Themis has in shop."},
		{"not in the profile", checkout, evidence.GapNotInProfile, evidence.ContainerLogsTail, "Container logs: not allowed by the rollout-regression profile. Add container_logs_tail to its evidence.allowed."},
		{"budget uses the profile's limit", checkout, evidence.GapBudgetSpent, evidence.ContainerLogsTail, "Container logs: not read. The investigation used all 2 of its evidence requests. Raise investigation.maxEvidenceRequests in the rollout-regression profile."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if m := writer.Message(tt.incident, store.EvidenceGap{Template: tt.template, Cause: tt.cause}); m != tt.want {
				t.Errorf("%q, want %q", m, tt.want)
			}
		})
	}
}

// One line per finding, however often it was recorded: evidence missing for
// the same reason shares a line, and a remedy is said once. Metrics read
// without a source say so, rather than repeat what the source said.
func TestMessages_OneLinePerFinding(t *testing.T) {
	notServed := "collect container_memory: no figures: " + metrics.ErrNotServed.Error() + "; " + metrics.NoHistory
	gaps := []store.EvidenceGap{
		{Template: evidence.PodStates, Cause: evidence.GapTimeout},
		{Template: evidence.PodStates, Cause: evidence.GapTimeout},
		{Template: evidence.ContainerMemory, Cause: evidence.GapUnavailable, Detail: notServed},
		{Template: evidence.ContainerCPU, Cause: evidence.GapUnavailable, Detail: "collect container_cpu: no figures: " + metrics.ErrNotServed.Error()},
		{Template: evidence.ContainerLogsTail, Cause: evidence.GapUnavailable, Detail: "collect container_logs_tail: " + metrics.ErrNotServed.Error()},
		{Template: evidence.ReferencedConfig, Cause: evidence.GapUnavailable, Detail: "collect referenced_config: connection refused."},
	}
	want := []string{
		"Pod states: Kubernetes didn't answer in time.",
		"Container memory, container CPU and container logs: no metrics source. Install metrics-server, or set metrics.prometheus.address.",
		"Referenced config: not read (connection refused).",
	}
	if got := writer.Messages(checkout, gaps); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	budget := []store.EvidenceGap{{Template: evidence.PodStates, Cause: evidence.GapBudgetSpent}, {Template: evidence.TargetEvents, Cause: evidence.GapForbidden}, {Template: evidence.ContainerLogsTail, Cause: evidence.GapBudgetSpent}}
	if got := writer.Messages(checkout, budget); len(got) != 2 || !strings.HasPrefix(got[0], "Pod states and container logs: not read. ") {
		t.Errorf("got %q, want the budget once, for both", got)
	}
}
