package governance_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// awaiting creates an incident whose action awaits approval, until
// config.DefaultApprovalTimeout from now.
func (e *env) awaiting(t *testing.T) store.Action {
	t.Helper()
	id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)
	return e.action(t, id)
}

// expireWorker sweeps an hour after the time to approve ran out.
func (e *env) expireWorker() *governance.ExpireWorker {
	return &governance.ExpireWorker{Store: e.store, Now: func() time.Time { return now.Add(config.DefaultApprovalTimeout + time.Hour) }}
}

func approvals(t *testing.T, result string) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_approvals_total", map[string]string{"result": result})
}

// An approval binds the change the approver was shown: another digest is
// refused and changes nothing. The right one authorizes the fix and queues it
// once, and approving again is refused as a replay.
func TestApprove(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	approved, mismatch, replayed := approvals(t, "approved"), approvals(t, "digest_mismatch"), approvals(t, "replayed")

	if _, err := governance.Approve(t.Context(), e.store, a.ID, "sha256:what-the-page-did-not-show", "alice", now); !errors.Is(err, store.ErrDigestMismatch) {
		t.Fatalf("approving another digest: %v, want a digest mismatch", err)
	}
	if got := e.state(t, a.IncidentID); got != incident.AwaitingApproval {
		t.Errorf("state = %s after a mismatch, want awaiting_approval", got)
	}
	if _, err := governance.Approve(t.Context(), e.store, a.ID, a.Digest, "alice", now); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := governance.Approve(t.Context(), e.store, a.ID, a.Digest, "alice", now); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Errorf("approving again: %v, want already decided", err)
	}
	if got := e.state(t, a.IncidentID); got != incident.Authorized {
		t.Errorf("state = %s, want authorized", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'actionID' = $2 AND args->>'incidentID' = $3`, governance.ExecuteArgs{}.Kind(), a.ID, a.IncidentID); n != 1 {
		t.Errorf("%d execute jobs for the action and its incident, want 1", n)
	}
	if a, m, r := approved(), mismatch(), replayed(); a != 1 || m != 1 || r != 1 {
		t.Errorf("counted %v approvals, %v mismatches and %v replays, want 1 each", a, m, r)
	}
}

// A denial closes the incident without acting, with the approver's reason on
// its timeline; denying again is refused as a replay.
func TestDeny(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	denied, replayed := approvals(t, "denied"), approvals(t, "replayed")

	if err := governance.Deny(t.Context(), e.store, a.ID, "alice", "not during the sale"); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if err := governance.Deny(t.Context(), e.store, a.ID, "alice", ""); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Errorf("denying again: %v, want already decided", err)
	}
	if got := e.state(t, a.IncidentID); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
	timeline, err := e.store.Transitions(t.Context(), a.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(timeline, func(tr store.Transition) bool {
		return tr.Reason == "Denied by alice: not during the sale" && tr.Actor == "alice"
	}) {
		t.Errorf("timeline %+v doesn't say alice denied it, and why", timeline)
	}
	if d, r := denied(), replayed(); d != 1 || r != 1 {
		t.Errorf("counted %v denials and %v replays, want 1 each", d, r)
	}
}

// Nobody answered: unlike a denial, the incident is still live. A second
// sweep finds nothing left to expire, so the expiry is counted once.
func TestExpire_Escalates(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	expired := approvals(t, "expired")
	w := e.expireWorker()
	for range 2 {
		if err := w.Expire(t.Context()); err != nil {
			t.Fatalf("Expire: %v", err)
		}
	}
	if got := e.state(t, a.IncidentID); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
	if got := expired(); got != 1 {
		t.Errorf("counted %v expiries, want 1", got)
	}
}

// One incident that moved on must not stop the sweep for every other one.
func TestExpire_ToleratesAnIncidentThatMovedOn(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE incidents SET state = 'self_resolved' WHERE id = $1`, a.IncidentID); err != nil {
		t.Fatal(err)
	}
	if err := e.expireWorker().Expire(t.Context()); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if got, _ := e.store.Action(t.Context(), e.store.DB(), a.ID); got.State != store.ActionExpired {
		t.Errorf("action = %s, want expired", got.State)
	}
}
