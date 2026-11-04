package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

func transitions(t *testing.T, from, to incident.State) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_incident_transitions_total", map[string]string{"from": string(from), "to": string(to)})
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

// Each committed transition is one info line with its incident and one count; a
// rolled-back one logs and counts nothing.
func TestTransition_Logged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(telemetry.NewLogger(&buf, "core", slog.LevelInfo))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s, _ := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-logged"))
	counted := transitions(t, incident.Received, incident.Investigating)
	_ = s.InTx(t.Context(), func(tx pgx.Tx) error {
		if err := s.Transition(t.Context(), tx, store.TransitionArgs{ID: id, From: incident.Received, To: incident.Investigating, Reason: "rolled back", Actor: "test"}); err != nil {
			return err
		}
		return errors.New("abort")
	})
	advance(t, s, id, incident.Received, incident.Investigating)
	if got := counted(); got != 1 {
		t.Errorf("counted %v transitions, want the committed one", got)
	}

	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		lines = append(lines, m)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %s", len(lines), buf.String())
	}
	l := lines[0]
	if l["msg"] != "incident transitioned" || l[telemetry.FieldIncidentID] != id || l["from"] != "received" || l["to"] != "investigating" {
		t.Errorf("line = %v", l)
	}
	if strings.Contains(buf.String(), "reason") {
		t.Error("the reason was logged")
	}
}

// gauges scrapes c alone and returns each series' value, by name and state.
func gauges(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			out[f.GetName()+state(m)] = m.GetGauge().GetValue()
		}
	}
	return out
}

func state(m *dto.Metric) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == "state" {
			return "/" + l.GetValue()
		}
	}
	return ""
}

func TestStateGauges_ReflectDatabase(t *testing.T) {
	s, pool := newStore(t)
	a := awaitingAction(t, s)
	if _, err := pool.Exec(t.Context(), `UPDATE actions SET created_at = now() - interval '90 seconds' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	createOrJoin(t, s, newIncident("fp-received"))
	other, _ := createOrJoin(t, s, newIncident("fp-closed"))
	advance(t, s, other, incident.Received, incident.Rejected)

	got := gauges(t, store.NewStateCollector(t.Context(), s))
	for name, want := range map[string]float64{
		"themis_incidents/investigating": 1,
		"themis_incidents/received":      1,
		"themis_incidents/rejected":      1,
		"themis_incidents/escalated":     0,
		"themis_approvals_pending":       1,
	} {
		if got[name] != want {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
	if age := got["themis_oldest_pending_approval_age_seconds"]; age < 90 || age > 3600 {
		t.Errorf("oldest pending approval age = %v, want about 90", age)
	}
	if n := len(got); n != len(incident.States())+2 {
		t.Errorf("%d series, want one per state and the two approval gauges", n)
	}
}

// A failed query reports nothing, so a scrape never shows stale or zero
// values as if they were current.
func TestStateGauges_FailedQueryReportsNothing(t *testing.T) {
	s, _ := newStore(t)
	errors := metrictest.Delta(t, "themis_metrics_collection_errors_total", nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if got := gauges(t, store.NewStateCollector(ctx, s)); len(got) != 0 {
		t.Errorf("a failed scrape reported %v", got)
	}
	if got := errors(); got != 1 {
		t.Errorf("counted %v collection errors, want 1", got)
	}
}
