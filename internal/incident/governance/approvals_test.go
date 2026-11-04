package governance_test

import (
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
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

// Nobody answered: unlike a denial, the incident is still live. A second
// sweep finds nothing left to expire, so the expiry is counted once.
func TestExpire_Escalates(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	expired := metrictest.Delta(t, "themis_approvals_total", map[string]string{"result": "expired"})
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
