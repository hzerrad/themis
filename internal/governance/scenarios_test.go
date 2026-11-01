package governance_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/store"
)

// shipped governs with the policy of the example configuration, so these
// scenarios check the rules Themis ships, not ones written for a test.
func shipped(t *testing.T) *env {
	t.Helper()
	example, err := config.Load("../../deploy/config/themis.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	p := profile("rollout-regression")
	p.Policy = example.Profiles[0].Policy
	w, err := governance.NewWorker(config.Config{EnrolledNamespaces: []string{e.target.Namespace}, Profiles: []config.Profile{p}}, e.store, k8s)
	if err != nil {
		t.Fatal(err)
	}
	w.Now = e.worker.Now
	e.worker = w
	return e
}

func facts(onset predicate.Truth) []predicate.Result {
	return []predicate.Result{
		{Name: predicate.NameOOMKilledNewRevision, Version: "1", Value: predicate.True, Reason: "x"},
		{Name: predicate.NameGitOpsManaged, Version: "1", Value: predicate.False, Reason: "x"},
		{Name: predicate.NameFailureOnsetAfterRollout, Version: "1", Value: onset, Reason: "x"},
	}
}

// Scenario 3: the crash loop began before the rollout. Rolling back would not
// help, so Themis does not blame the rollout: nothing is changed.
func TestScenario_CrashLoopOlderThanTheRolloutIsNotRolledBack(t *testing.T) {
	e := shipped(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, facts(predicate.False))
	e.govern(t, p)
	assertNotActed(t, e, id)
}

// Scenario 6: the evidence that would say when the failures started is
// missing. Without it, no rollback is allowed.
func TestScenario_MissingEvidenceAllowsNoAction(t *testing.T) {
	e := shipped(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, facts(predicate.Unknown))
	e.govern(t, p)
	assertNotActed(t, e, id)
}

// The control: a bad rollout, with every fact known, waits for approval.
func TestScenario_BadRolloutWaitsForApproval(t *testing.T) {
	e := shipped(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, facts(predicate.True))
	e.govern(t, p)
	if got := e.state(t, id); got != incident.AwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval", got)
	}
}

func assertNotActed(t *testing.T, e *env, id string) {
	t.Helper()
	if got := e.state(t, id); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
	a := e.action(t, id)
	if a.State != store.ActionDenied || a.DecisionReason != "Policy rule rollback-needs-a-bad-rollout forbids this fix." {
		t.Errorf("action = %s: %q, want denied by rollback-needs-a-bad-rollout", a.State, a.DecisionReason)
	}
}
