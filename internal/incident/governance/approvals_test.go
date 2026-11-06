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

	if _, err := governance.Approve(t.Context(), e.store, a.ID, "sha256:what-the-page-did-not-show", "alice", now, false); !errors.Is(err, store.ErrDigestMismatch) {
		t.Fatalf("approving another digest: %v, want a digest mismatch", err)
	}
	if got := e.state(t, a.IncidentID); got != incident.AwaitingApproval {
		t.Errorf("state = %s after a mismatch, want awaiting_approval", got)
	}
	if _, err := governance.Approve(t.Context(), e.store, a.ID, a.Digest, "alice", now, false); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := governance.Approve(t.Context(), e.store, a.ID, a.Digest, "alice", now, false); !errors.Is(err, store.ErrAlreadyDecided) {
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

// A denial keeps the incident open for another fix, with the approver's
// reason on its timeline as they wrote it, and kept for the investigator.
// Denying or approving again is refused as a replay.
func TestDeny(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	denied, replayed := approvals(t, "denied"), approvals(t, "replayed")

	if err := governance.Deny(t.Context(), e.store, a.ID, "alice", "  not during the sale\n", now); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if err := governance.Deny(t.Context(), e.store, a.ID, "alice", "", now); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Errorf("denying again: %v, want already decided", err)
	}
	if err := governance.DenyAndClose(t.Context(), e.store, a.ID, "alice", "", now); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Errorf("denying and closing after a denial: %v, want already decided", err)
	}
	if _, err := governance.Approve(t.Context(), e.store, a.ID, a.Digest, "bob", now, false); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Errorf("approving after a denial: %v, want already decided", err)
	}
	if got := e.state(t, a.IncidentID); got != incident.AwaitingSuggestion {
		t.Errorf("state = %s, want awaiting_suggestion", got)
	}
	e.says(t, a.IncidentID, "alice", "Denied by alice. Reason: not during the sale.")
	got, err := e.store.Denials(t.Context(), e.store.DB(), a.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Denier != "alice" || got[0].Reason != "not during the sale" ||
		got[0].Capability != a.Capability || got[0].Params["revision"] != int64(1) || !got[0].At.Equal(now) {
		t.Errorf("denials = %+v, want alice's of the rollback to revision 1, with her reason", got)
	}
	if d, r := denied(), replayed(); d != 1 || r != 3 {
		t.Errorf("counted %v denials and %v replays, want 1 and 3", d, r)
	}
}

// Denying and closing ends the incident with no change. A reason that ends
// its own sentence keeps its punctuation.
func TestDenyAndClose(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if err := governance.DenyAndClose(t.Context(), e.store, a.ID, "alice", "Not during the sale!", now); err != nil {
		t.Fatalf("DenyAndClose: %v", err)
	}
	if got := e.state(t, a.IncidentID); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
	e.says(t, a.IncidentID, "alice", "Denied by alice. Reason: Not during the sale!")
}

// says fails unless the step out of awaiting approval says reason, by actor.
func (e *env) says(t *testing.T, incidentID, actor, reason string) {
	t.Helper()
	timeline, err := e.store.Transitions(t.Context(), incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(timeline, func(tr store.Transition) bool {
		return tr.Reason == reason && tr.Actor == actor && tr.From == incident.AwaitingApproval
	}) {
		t.Errorf("timeline %+v doesn't say %q, by %s", timeline, reason, actor)
	}
}

// suggestedBy makes the proposal one that source suggested from outside
// Themis.
func (e *env) suggestedBy(t *testing.T, proposalID, source string) {
	t.Helper()
	const sql = `UPDATE proposals SET lease_id = NULL, provenance = jsonb_build_object('source', $2::text) WHERE id = $1`
	if _, err := e.pool.Exec(t.Context(), sql, proposalID, source); err != nil {
		t.Fatal(err)
	}
}

// By default a person may approve a fix they suggested, and the timeline
// says they did both.
func TestApprove_OwnSuggestion(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	e.suggestedBy(t, a.ProposalID, "person:alice")
	if _, err := governance.Approve(t.Context(), e.store, a.ID, a.Digest, "alice", now, false); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got := e.state(t, a.IncidentID); got != incident.Authorized {
		t.Errorf("state = %s, want authorized", got)
	}
	e.says(t, a.IncidentID, "alice", "Suggested and approved by alice.")
}

// With another approver required, the person who suggested a fix can't
// approve it, and nothing changes; someone else can. Only a person's own
// suggestion counts: the admin token's, or the investigator's, doesn't.
func TestApprove_AnotherApprover(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	e.suggestedBy(t, a.ProposalID, "person:alice")
	if _, err := governance.Approve(t.Context(), e.store, a.ID, a.Digest, "alice", now, true); !errors.Is(err, governance.ErrOwnSuggestion) {
		t.Fatalf("approving her own suggestion: %v, want refused", err)
	}
	if got := e.state(t, a.IncidentID); got != incident.AwaitingApproval {
		t.Errorf("state = %s after the refusal, want awaiting_approval", got)
	}
	if _, err := governance.Approve(t.Context(), e.store, a.ID, a.Digest, "bob", now, true); err != nil {
		t.Fatalf("Approve by bob: %v", err)
	}
	e.says(t, a.IncidentID, "bob", "Approved by bob.")

	for _, source := range []string{"admin-token", ""} {
		other := newEnv(t)
		b := other.awaiting(t)
		if source != "" {
			other.suggestedBy(t, b.ProposalID, source)
		}
		if _, err := governance.Approve(t.Context(), other.store, b.ID, b.Digest, "admin", now, true); err != nil {
			t.Errorf("approving a fix from %q: %v", source, err)
		}
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
