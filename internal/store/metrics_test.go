package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

func transitions(t *testing.T, from, to incident.State) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_incident_transitions_total", map[string]string{"from": string(from), "to": string(to)})
}

func TestTransition_Counted(t *testing.T) {
	s, _ := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-counted"))
	counted := transitions(t, incident.Received, incident.Investigating)
	advance(t, s, id, incident.Received, incident.Investigating)
	if got := counted(); got != 1 {
		t.Errorf("counted %v transitions, want 1", got)
	}
}

func TestTransition_RolledBack_NotCounted(t *testing.T) {
	s, _ := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-rolled-back"))
	counted := transitions(t, incident.Received, incident.Investigating)
	errAbort := errors.New("abort")
	err := s.InTx(t.Context(), func(tx pgx.Tx) error {
		if err := s.Transition(t.Context(), tx, store.TransitionArgs{ID: id, From: incident.Received, To: incident.Investigating, Reason: "test", Actor: "test"}); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("InTx: %v", err)
	}
	if got := counted(); got != 0 {
		t.Errorf("counted %v rolled-back transitions, want 0", got)
	}
}

func TestTransition_CountsEscalationsAndStages(t *testing.T) {
	s, _ := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-escalated"))
	escalated := metrictest.Delta(t, "themis_escalations_total", map[string]string{"from": string(incident.Executing)})
	stages := map[string]uint64{}
	for _, st := range []string{"to_proposal", "approval_wait", "execution", "verification", "to_close"} {
		stages[st] = metrictest.Count(t, "themis_stage_duration_seconds", map[string]string{"stage": st})
	}

	advance(t, s, id, incident.Received, incident.Investigating, incident.Diagnosed, incident.Proposed,
		incident.AwaitingApproval, incident.Authorized, incident.Executing, incident.Escalated)

	if got := escalated(); got != 1 {
		t.Errorf("counted %v escalations from executing, want 1", got)
	}
	for st, want := range map[string]uint64{"to_proposal": 1, "approval_wait": 1, "execution": 1, "verification": 0, "to_close": 1} {
		if got := metrictest.Count(t, "themis_stage_duration_seconds", map[string]string{"stage": st}) - stages[st]; got != want {
			t.Errorf("stage %s: %d observations, want %d", st, got, want)
		}
	}
}

// Time in a state runs from entering it: an alert joining the incident
// meanwhile does not restart it.
func TestTransition_StageDurationSurvivesJoins(t *testing.T) {
	s, pool := newStore(t)
	in := newIncident("fp-joined")
	id, _ := createOrJoin(t, s, in)
	advance(t, s, id, incident.Received, incident.Investigating, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval)
	if _, err := pool.Exec(t.Context(), `UPDATE transitions SET at = now() - interval '100 seconds' WHERE incident_id = $1 AND to_state = 'awaiting_approval'`, id); err != nil {
		t.Fatal(err)
	}
	if _, joined := createOrJoin(t, s, in); joined {
		t.Fatal("the alert opened a new incident")
	}

	labels := map[string]string{"stage": "approval_wait"}
	before := metrictest.Sum(t, "themis_stage_duration_seconds", labels)
	advance(t, s, id, incident.AwaitingApproval, incident.Authorized)
	if got := metrictest.Sum(t, "themis_stage_duration_seconds", labels) - before; got < 100 || got > 200 {
		t.Errorf("approval_wait observed %vs, want about 100", got)
	}
}
