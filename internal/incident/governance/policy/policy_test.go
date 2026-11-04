package policy_test

import (
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
)

func input() policy.Input {
	return policy.Input{
		Capability: "deployment.rollback-to-revision", Risk: "medium",
		Namespace: "shop", ManagedBy: "",
		Predicates: map[string]string{"oom_killed_new_revision": "true", "gitops_managed": "false"},
	}
}

func compile(t *testing.T, mode string, rules ...config.PolicyRule) *policy.Policy {
	t.Helper()
	p, err := policy.Compile(config.Policy{Mode: mode, Rules: rules})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}

func rule(name, when, decision string) config.PolicyRule {
	return config.PolicyRule{Name: name, When: when, Decision: decision}
}

func TestPolicy_FirstMatchingRuleDecides(t *testing.T) {
	p := compile(t, config.ModeGuarded,
		rule("no-gitops", `predicates.gitops_managed != "false"`, config.DecisionDeny),
		rule("approve-rollbacks", `action.capability == "deployment.rollback-to-revision"`, config.DecisionRequireApproval),
	)
	d := p.Evaluate(input())
	if d.Outcome != policy.RequireApproval || d.PolicyID != "approve-rollbacks" || d.PolicyDigest == "" {
		t.Errorf("decision = %+v", d)
	}

	in := input()
	in.Predicates["gitops_managed"] = "true"
	if d := p.Evaluate(in); d.Outcome != policy.Deny || d.PolicyID != "no-gitops" {
		t.Errorf("gitops-managed decision = %+v", d)
	}
}

// A rule that fails to evaluate denies; it never lets an action through. A
// rule that matches and denies has not failed.
func TestPolicy_EvaluationErrorDenies(t *testing.T) {
	p := compile(t, config.ModeGuarded,
		rule("missing-key", `predicates.not_computed == "true"`, config.DecisionAllow),
	)
	if d := p.Evaluate(input()); d.Outcome != policy.Deny || !d.Failed || !strings.Contains(d.Reason, "missing-key") {
		t.Errorf("decision = %+v, want a failed deny", d)
	}
	p = compile(t, config.ModeGuarded, rule("deny-all", `true`, config.DecisionDeny))
	if d := p.Evaluate(input()); d.Outcome != policy.Deny || d.Failed {
		t.Errorf("decision = %+v, want a deny that did not fail", d)
	}
}

func TestCompile_RejectsInvalidRules(t *testing.T) {
	for name, r := range map[string]config.PolicyRule{
		"syntax":      rule("r", "action.capability ==", config.DecisionDeny),
		"not boolean": rule("r", "action.capability", config.DecisionDeny),
		"unknown var": rule("r", `cluster.admin == true`, config.DecisionDeny),
	} {
		if _, err := policy.Compile(config.Policy{Rules: []config.PolicyRule{r}}); err == nil {
			t.Errorf("%s: Compile accepted %q", name, r.When)
		}
	}
}

func TestPolicy_DigestFollowsRules(t *testing.T) {
	a := compile(t, config.ModeGuarded, rule("r", "true", config.DecisionDeny))
	b := compile(t, config.ModeGuarded, rule("r", "false", config.DecisionDeny))
	if a.Evaluate(input()).PolicyDigest == b.Evaluate(input()).PolicyDigest {
		t.Error("different rules share a digest")
	}
}

// A decision's reason is what the approver reads: which rule decided, in words.
// Guarded mode asks a person even when a rule allows, and observe mode denies
// everything.
func TestPolicy_ReasonsArePlainSentences(t *testing.T) {
	for _, c := range []struct {
		mode     string
		decision string
		outcome  policy.Outcome
		want     string
	}{
		{config.ModeGuarded, config.DecisionDeny, policy.Deny, "Policy rule r forbids this fix."},
		{config.ModeGuarded, config.DecisionRequireApproval, policy.RequireApproval, "Policy rule r requires a person's approval."},
		{config.ModeGuarded, config.DecisionAllow, policy.RequireApproval, "Policy rule r allows this fix. In guarded mode, a person must approve every fix."},
		{config.ModeObserve, config.DecisionAllow, policy.Deny, "Policy rule r allows this fix. Themis is in observe mode, so it never makes changes."},
	} {
		if d := compile(t, c.mode, rule("r", `true`, c.decision)).Evaluate(input()); d.Outcome != c.outcome || d.Reason != c.want {
			t.Errorf("%s, %s: %s, %q; want %s, %q", c.mode, c.decision, d.Outcome, d.Reason, c.outcome, c.want)
		}
	}
	if got := compile(t, config.ModeGuarded).Evaluate(input()).Reason; got != "No policy rule matched, so a person must approve this fix." {
		t.Errorf("no rule: reason = %q", got)
	}
}

func restore() policy.Input {
	in := input()
	in.Restore = true
	return in
}

// Putting back what Themis changed needs no approval by default, even in
// guarded mode: it returns the workload to how a person left it.
func TestPolicy_RestoreAllowedByDefault(t *testing.T) {
	d := compile(t, config.ModeGuarded).Evaluate(restore())
	if d.Outcome != policy.Allow || d.Reason != "No policy rule matched, so Themis may put the workload back as it was." {
		t.Errorf("decision = %+v, want allowed", d)
	}
	if d := compile(t, config.ModeGuarded).Evaluate(input()); d.Outcome != policy.RequireApproval {
		t.Errorf("a fix with no rule = %s, want require_approval", d.Outcome)
	}
}

// A rule can still ask a person, or forbid it.
func TestPolicy_RulesSeeRestores(t *testing.T) {
	p := compile(t, config.ModeGuarded, rule("ask-first", `action.restore == "true"`, config.DecisionRequireApproval))
	if d := p.Evaluate(restore()); d.Outcome != policy.RequireApproval {
		t.Errorf("restore = %s, want require_approval", d.Outcome)
	}
	if d := p.Evaluate(input()); d.Outcome != policy.RequireApproval || d.PolicyID == "ask-first" {
		t.Errorf("a fix matched the restore rule: %+v", d)
	}
}

// Observe mode never acts, not even to put things back.
func TestPolicy_ObserveModeNeverRestores(t *testing.T) {
	if d := compile(t, config.ModeObserve).Evaluate(restore()); d.Outcome != policy.Deny {
		t.Errorf("observe mode restore = %s, want deny", d.Outcome)
	}
}

// Rules can tell who suggested a fix, and whether that is proven.
func TestPolicy_SeesWhoSuggestedTheFix(t *testing.T) {
	p := compile(t, config.ModeGuarded,
		rule("no-unproven-outsiders", `proposal.external == "true" && proposal.authenticated != "true"`, config.DecisionDeny),
		rule("this-agent-only", `proposal.external == "true" && proposal.source != "serviceaccount:agents/triage"`, config.DecisionDeny))
	tests := []struct {
		name string
		in   policy.Input
		want policy.Outcome
	}{
		{"the investigator", policy.Input{Source: "themis-intelligence", Authenticated: true}, policy.RequireApproval},
		{"the named agent", policy.Input{Source: "serviceaccount:agents/triage", Authenticated: true, External: true}, policy.RequireApproval},
		{"another agent", policy.Input{Source: "serviceaccount:agents/other", Authenticated: true, External: true}, policy.Deny},
		{"not proven", policy.Input{Source: "serviceaccount:agents/triage", External: true}, policy.Deny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Evaluate(tt.in).Outcome; got != tt.want {
				t.Errorf("outcome = %s, want %s", got, tt.want)
			}
		})
	}
}
