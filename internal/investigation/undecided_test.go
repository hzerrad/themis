package investigation_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/investigation"
	"github.com/hzerrad/themis/internal/store"
)

func (e *env) undecided(t *testing.T) string {
	t.Helper()
	id := e.newIncident(t, "p", e.target)
	for _, step := range [][2]incident.State{{incident.Received, incident.Investigating}, {incident.Investigating, incident.Inconclusive}} {
		err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
			return e.store.Transition(t.Context(), tx, store.TransitionArgs{ID: id, From: step[0], To: step[1], Reason: "x", Actor: "test"})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestCloseUndecided_WaitsOutTheWindow(t *testing.T) {
	e := newEnv(t)
	id := e.undecided(t)
	window := config.Config{ExternalProposals: config.ExternalProposals{Window: config.Duration{Duration: time.Hour}}}

	early := &investigation.CloseUndecidedWorker{Config: window, Store: e.store, Now: func() time.Time { return time.Now().Add(30 * time.Minute) }}
	if err := early.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, id); got != incident.Inconclusive {
		t.Fatalf("state = %s, want it still open", got)
	}

	late := &investigation.CloseUndecidedWorker{Config: window, Store: e.store, Now: func() time.Time { return time.Now().Add(2 * time.Hour) }}
	if err := late.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, id); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed", got)
	}
}

// With the window gone from the configuration, nothing holds an incident open.
func TestCloseUndecided_WithoutAWindowClosesAtOnce(t *testing.T) {
	e := newEnv(t)
	id := e.undecided(t)
	w := &investigation.CloseUndecidedWorker{Config: config.Config{}, Store: e.store, Now: func() time.Time { return time.Now().Add(time.Second) }}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, id); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed", got)
	}
}

// Alerts that keep firing don't keep an incident open: the window runs from
// when Themis couldn't decide.
func TestCloseUndecided_RepeatAlertsDontExtendTheWindow(t *testing.T) {
	e := newEnv(t)
	id := e.undecided(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE transitions SET at = now() - interval '2 hours' WHERE incident_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	e.newIncident(t, "p", e.target) // joins it, as a repeat alert does
	window := config.Config{ExternalProposals: config.ExternalProposals{Window: config.Duration{Duration: time.Hour}}}
	w := &investigation.CloseUndecidedWorker{Config: window, Store: e.store}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, id); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed", got)
	}
}
