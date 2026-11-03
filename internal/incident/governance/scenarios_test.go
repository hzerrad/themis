package governance_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/store"
)

// shipped governs with the policy of the example configuration, so these
// scenarios check the rules Themis ships, not ones written for a test.
func shipped(t *testing.T) *env {
	t.Helper()
	example, err := config.Load("../../../deploy/config/themis.example.yaml")
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

func facts(onset check.Truth) []check.Result {
	return []check.Result{
		{Name: check.NameOOMKilledNewRevision, Version: "1", Value: check.True, Reason: "x"},
		{Name: check.NameGitOpsManaged, Version: "1", Value: check.False, Reason: "x"},
		{Name: check.NameFailureOnsetAfterRollout, Version: "1", Value: onset, Reason: "x"},
		{Name: check.NameConfigChangedBeforeFailure, Version: "1", Value: check.False, Reason: "x"},
		{Name: check.NameRolloutRecent, Version: "1", Value: check.True, Reason: "x"},
		{Name: check.NamePriorRevisionWasHealthy, Version: "1", Value: check.True, Reason: "x"},
	}
}

// Scenario 3: the crash loop began before the rollout. Rolling back would not
// help, so Themis does not blame the rollout: nothing is changed.
func TestScenario_CrashLoopOlderThanTheRolloutIsNotRolledBack(t *testing.T) {
	e := shipped(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, facts(check.False))
	e.govern(t, p)
	assertNotActed(t, e, id)
}

// Scenario 6: the evidence that would say when the failures started is
// missing. Without it, no rollback is allowed.
func TestScenario_MissingEvidenceAllowsNoAction(t *testing.T) {
	e := shipped(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, facts(check.Unknown))
	e.govern(t, p)
	assertNotActed(t, e, id)
}

// A referenced ConfigMap changed between the rollout and the failures, so the
// rollout may not be to blame, and rolling back wouldn't undo the change.
func TestScenario_ConfigChangeBeforeTheFailureIsNotRolledBack(t *testing.T) {
	for _, config := range []check.Truth{check.True, check.Unknown} {
		e := shipped(t)
		results := facts(check.True)
		results[3].Value = config
		id, p := e.diagnosed(t, "rollout-regression", 1, results)
		e.govern(t, p)
		assertNotActed(t, e, id)
	}
}

// A failure long after the rollout isn't blamed on it, however it looks.
func TestScenario_OldRolloutIsNotRolledBack(t *testing.T) {
	e := shipped(t)
	results := facts(check.True)
	results[4].Value = check.False
	id, p := e.diagnosed(t, "rollout-regression", 1, results)
	e.govern(t, p)
	assertNotActed(t, e, id)
}

// A new version that crashes, or never starts, while the one before it was
// healthy is a bad rollout too, though nothing ran out of memory.
func TestScenario_BadRolloutWithoutOOMWaitsForApproval(t *testing.T) {
	e := shipped(t)
	results := facts(check.True)
	results[0].Value = check.False
	id, p := e.diagnosed(t, "rollout-regression", 1, results)
	e.govern(t, p)
	if got := e.state(t, id); got != incident.AwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval", got)
	}
}

// With no memory kill and no healthy version to go back to, nothing says the
// rollout is to blame, so it isn't rolled back.
func TestScenario_NoSignOfABadRevisionIsNotRolledBack(t *testing.T) {
	for _, prior := range []check.Truth{check.False, check.Unknown} {
		e := shipped(t)
		results := facts(check.True)
		results[0].Value, results[5].Value = check.False, prior
		id, p := e.diagnosed(t, "rollout-regression", 1, results)
		e.govern(t, p)
		assertNotActed(t, e, id)
	}
}

// The control: a bad rollout, with every fact known, waits for approval.
func TestScenario_BadRolloutWaitsForApproval(t *testing.T) {
	e := shipped(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, facts(check.True))
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
