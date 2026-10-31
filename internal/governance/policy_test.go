package governance_test

import (
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/governance"
)

func input() governance.PolicyInput {
	return governance.PolicyInput{
		Capability: "deployment.rollback-to-revision", Risk: "medium",
		Namespace: "shop", ManagedBy: "",
		Predicates: map[string]string{"oom_killed_new_revision": "true", "gitops_managed": "false"},
	}
}

func compile(t *testing.T, mode string, rules ...config.PolicyRule) *governance.Policy {
	t.Helper()
	p, err := governance.CompilePolicy(config.Policy{Mode: mode, Rules: rules})
	if err != nil {
		t.Fatalf("CompilePolicy: %v", err)
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
	if d.Outcome != governance.RequireApproval || d.PolicyID != "approve-rollbacks" || d.PolicyDigest == "" {
		t.Errorf("decision = %+v", d)
	}

	in := input()
	in.Predicates["gitops_managed"] = "true"
	if d := p.Evaluate(in); d.Outcome != governance.Deny || d.PolicyID != "no-gitops" {
		t.Errorf("gitops-managed decision = %+v", d)
	}
}

func TestPolicy_RequiresApprovalInGuarded(t *testing.T) {
	p := compile(t, config.ModeGuarded, rule("allow-all", "true", config.DecisionAllow))
	if d := p.Evaluate(input()); d.Outcome != governance.RequireApproval || !strings.Contains(d.Reason, "guarded mode") {
		t.Errorf("decision = %+v, want require_approval in guarded mode", d)
	}
}

func TestPolicy_ObserveDeniesEverything(t *testing.T) {
	p := compile(t, config.ModeObserve, rule("allow-all", "true", config.DecisionAllow))
	if d := p.Evaluate(input()); d.Outcome != governance.Deny {
		t.Errorf("decision = %+v, want deny in observe mode", d)
	}
}

func TestPolicy_NoRuleMatchedNeedsApproval(t *testing.T) {
	p := compile(t, config.ModeGuarded)
	if d := p.Evaluate(input()); d.Outcome != governance.RequireApproval {
		t.Errorf("decision = %+v", d)
	}
}

func TestPolicy_UnknownPredicateBlocks(t *testing.T) {
	p := compile(t, config.ModeGuarded,
		rule("needs-oom", `predicates.oom_killed_new_revision != "true"`, config.DecisionDeny),
		rule("approve", "true", config.DecisionRequireApproval),
	)
	in := input()
	in.Predicates["oom_killed_new_revision"] = "unknown"
	if d := p.Evaluate(in); d.Outcome != governance.Deny {
		t.Errorf("decision = %+v, want deny on unknown", d)
	}
}

// A rule that fails to evaluate denies; it never lets an action through.
func TestPolicy_EvaluationErrorDenies(t *testing.T) {
	p := compile(t, config.ModeGuarded,
		rule("missing-key", `predicates.not_computed == "true"`, config.DecisionAllow),
	)
	if d := p.Evaluate(input()); d.Outcome != governance.Deny || !d.Failed || !strings.Contains(d.Reason, "missing-key") {
		t.Errorf("decision = %+v, want a failed deny", d)
	}
}

func TestPolicy_MatchedRuleIsNotFailed(t *testing.T) {
	p := compile(t, config.ModeGuarded, rule("deny-all", `true`, config.DecisionDeny))
	if d := p.Evaluate(input()); d.Outcome != governance.Deny || d.Failed {
		t.Errorf("decision = %+v, want a deny that did not fail", d)
	}
}

func TestCompilePolicy_RejectsInvalidRules(t *testing.T) {
	for name, r := range map[string]config.PolicyRule{
		"syntax":      rule("r", "action.capability ==", config.DecisionDeny),
		"not boolean": rule("r", "action.capability", config.DecisionDeny),
		"unknown var": rule("r", `cluster.admin == true`, config.DecisionDeny),
	} {
		if _, err := governance.CompilePolicy(config.Policy{Rules: []config.PolicyRule{r}}); err == nil {
			t.Errorf("%s: CompilePolicy accepted %q", name, r.When)
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
func TestPolicy_ReasonsArePlainSentences(t *testing.T) {
	for _, c := range []struct {
		mode     string
		decision string
		want     string
	}{
		{config.ModeGuarded, config.DecisionDeny, "Policy rule r forbids this fix."},
		{config.ModeGuarded, config.DecisionRequireApproval, "Policy rule r requires a person's approval."},
		{config.ModeGuarded, config.DecisionAllow, "Policy rule r allows this fix. In guarded mode, a person must approve every fix."},
		{config.ModeObserve, config.DecisionAllow, "Policy rule r allows this fix. Themis is in observe mode, so it never makes changes."},
	} {
		if got := compile(t, c.mode, rule("r", `true`, c.decision)).Evaluate(input()).Reason; got != c.want {
			t.Errorf("%s, %s: reason = %q, want %q", c.mode, c.decision, got, c.want)
		}
	}
	if got := compile(t, config.ModeGuarded).Evaluate(input()).Reason; got != "No policy rule matched, so a person must approve this fix." {
		t.Errorf("no rule: reason = %q", got)
	}
}
