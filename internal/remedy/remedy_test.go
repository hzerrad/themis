package remedy_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/remedy"
	"github.com/hzerrad/themis/internal/store"
)

var (
	writer = remedy.Writer{Config: config.Config{
		EnrolledNamespaces: []string{"shop"},
		Profiles:           []config.Profile{{Name: "rollout-regression", Investigation: config.Investigation{MaxEvidenceRequests: 2}}},
	}}
	checkout = store.Incident{Profile: "rollout-regression", Target: incident.TargetRef{Namespace: "shop", Name: "checkout"}}
)

func message(cause evidence.GapCause, template string) string {
	return writer.Message(checkout, store.EvidenceGap{Template: template, Cause: cause, Detail: "connection refused"})
}

var templates = []string{evidence.RolloutHistory, evidence.PodStates, evidence.TargetEvents, evidence.ContainerLogsTail, evidence.ReferencedConfig}

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
	for _, h := range []string{remedy.Headline(true), remedy.Headline(false)} {
		for _, w := range internalWords {
			if regexp.MustCompile(`\b` + w).MatchString(strings.ToLower(h)) {
				t.Errorf("headline %q uses %q", h, w)
			}
		}
	}
}

// Plural evidence takes plural words: "Container logs ... them", never "it".
func TestMessages_AgreeInNumber(t *testing.T) {
	for _, c := range evidence.GapCauses() {
		if m := message(c, evidence.ContainerLogsTail); strings.Contains(m, "read it") || strings.Contains(m, " was ") || strings.Contains(m, "wasn't") {
			t.Errorf("%s: %q treats the logs as one thing", c, m)
		}
		if c == evidence.GapExpired {
			continue // only events expire; that message speaks of them
		}
		if m := message(c, evidence.RolloutHistory); strings.Contains(m, "them") || strings.Contains(m, "weren't") || strings.Contains(m, " were ") {
			t.Errorf("%s: %q treats the history as many", c, m)
		}
	}
}

func TestRemedy_ForbiddenNamesTheNamespace(t *testing.T) {
	away := checkout
	away.Target.Namespace = "billing"
	if m := writer.Message(away, store.EvidenceGap{Template: evidence.RolloutHistory, Cause: evidence.GapForbidden}); !strings.Contains(m, "add billing to enrolledNamespaces") {
		t.Errorf("not enrolled: %q", m)
	}
	if m := message(evidence.GapForbidden, evidence.RolloutHistory); !strings.Contains(m, "Check the Role Themis has in shop") {
		t.Errorf("enrolled but forbidden: %q", m)
	}
}

func TestRemedy_NotInProfileNamesTheTemplateAndProfile(t *testing.T) {
	m := message(evidence.GapNotInProfile, evidence.ContainerLogsTail)
	if !strings.Contains(m, "rollout-regression profile") || !strings.Contains(m, "add container_logs_tail to that profile's evidence.allowed") {
		t.Errorf("%q", m)
	}
}

func TestRemedy_BudgetUsesTheProfilesLimit(t *testing.T) {
	if m := message(evidence.GapBudgetSpent, evidence.ContainerLogsTail); !strings.Contains(m, "all 2 of its evidence requests") {
		t.Errorf("%q", m)
	}
}

// One message per thing missed, however often it was recorded.
func TestMessages_OncePerMessage(t *testing.T) {
	g := store.EvidenceGap{Template: evidence.PodStates, Cause: evidence.GapTimeout}
	if got := writer.Messages(checkout, []store.EvidenceGap{g, g}); len(got) != 1 {
		t.Errorf("got %v", got)
	}
}
