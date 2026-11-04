package governance_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

// shipped governs with the policy of the rollout-regression scenario pack, so
// these scenarios check the rules Themis ships, not ones written for a test.
func shipped(t *testing.T) *env {
	t.Helper()
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	p := profile("rollout-regression")
	p.Policy = cat.Profiles[0].Policy
	w := &governance.Worker{Config: config.Config{EnrolledNamespaces: []string{e.target.Namespace}, Profiles: []config.Profile{p}}, Store: e.store, Reader: k8s, Fixes: catalog.Fixes()}
	w.Now = e.worker.Now
	e.worker = w
	return e
}

func facts(onset check.Truth) []check.Result {
	return []check.Result{
		{Name: "oom_killed_new_revision", Version: "1", Value: check.True, Reason: "x"},
		{Name: check.NameGitOpsManaged, Version: "1", Value: check.False, Reason: "x"},
		{Name: "failure_onset_after_rollout", Version: "1", Value: onset, Reason: "x"},
		{Name: "config_changed_before_failure", Version: "1", Value: check.False, Reason: "x"},
		{Name: "rollout_recent", Version: "1", Value: check.True, Reason: "x"},
		{Name: "prior_revision_was_healthy", Version: "1", Value: check.True, Reason: "x"},
	}
}

// set returns facts with the result at i changed to v.
func set(results []check.Result, i int, v check.Truth) []check.Result {
	results[i].Value = v
	return results
}

// The policy Themis ships rolls back only a rollout that is to blame, and even
// then only with a person's approval.
func TestScenario_ShippedPolicy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		results []check.Result
		acted   bool
	}{
		// The control: a bad rollout, with every fact known, waits for approval.
		{"bad rollout", facts(check.True), true},
		// A new version that crashes, or never starts, while the one before it
		// was healthy is a bad rollout too, though nothing ran out of memory.
		{"bad rollout without OOM", set(facts(check.True), 0, check.False), true},
		// Scenario 3: the crash loop began before the rollout. Rolling back
		// would not help, so Themis does not blame the rollout.
		{"crash loop older than the rollout", facts(check.False), false},
		// Scenario 6: the evidence that would say when the failures started is
		// missing. Without it, no rollback is allowed.
		{"missing evidence", facts(check.Unknown), false},
		// A referenced ConfigMap changed between the rollout and the failures,
		// so the rollout may not be to blame, and rolling back wouldn't undo
		// the change.
		{"config changed before the failure", set(facts(check.True), 3, check.True), false},
		{"config change unknown", set(facts(check.True), 3, check.Unknown), false},
		// A failure long after the rollout isn't blamed on it, however it looks.
		{"old rollout", set(facts(check.True), 4, check.False), false},
		// With no memory kill and no healthy version to go back to, nothing
		// says the rollout is to blame.
		{"no sign of a bad revision", set(set(facts(check.True), 0, check.False), 5, check.False), false},
		{"prior revision's health unknown", set(set(facts(check.True), 0, check.False), 5, check.Unknown), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := shipped(t)
			refusals := metrictest.Delta(t, "themis_gitops_refusals_total", nil)
			id, p := e.diagnosed(t, "rollout-regression", 1, tt.results)
			e.govern(t, p)
			if tt.acted {
				if got := e.state(t, id); got != incident.AwaitingApproval {
					t.Errorf("state = %s, want awaiting_approval", got)
				}
				return
			}
			assertNotActed(t, e, id)
			// A denial of a target no GitOps controller manages is not a
			// GitOps refusal.
			if got := refusals(); got != 0 {
				t.Errorf("counted %v GitOps refusals, want 0", got)
			}
		})
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
