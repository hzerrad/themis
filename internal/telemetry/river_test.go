package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type incidentArgs struct{ IncidentID string }

func (incidentArgs) Kind() string       { return "about_incident" }
func (a incidentArgs) Incident() string { return a.IncidentID }

type sweepArgs struct{}

func (sweepArgs) Kind() string { return "sweep" }

// logWorker logs one line with the context it is given.
type logWorker[T river.JobArgs] struct {
	river.WorkerDefaults[T]
	log *slog.Logger
}

func (w logWorker[T]) Work(ctx context.Context, _ *river.Job[T]) error {
	w.log.InfoContext(ctx, "worked")
	return nil
}

func work[T river.JobArgs](t *testing.T, wrap func(river.Worker[T]) river.Worker[T], args T) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	w := wrap(logWorker[T]{log: NewLogger(&buf, "core", slog.LevelInfo)})
	if err := w.Work(t.Context(), &river.Job[T]{JobRow: &rivertype.JobRow{Kind: args.Kind()}, Args: args}); err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not one JSON line: %q", buf.String())
	}
	return line
}

// Every job's lines name the job, and the incident when its args carry one.
func TestWorkers_LogTheJob(t *testing.T) {
	incident := func(w river.Worker[incidentArgs]) river.Worker[incidentArgs] { return incidentWorker[incidentArgs]{w} }
	for name, tc := range map[string]struct {
		line     map[string]any
		job      string
		incident any
	}{
		"about an incident": {work(t, incident, incidentArgs{IncidentID: "inc-1"}), "about_incident", "inc-1"},
		// A job queued before its args carried the incident logs no empty field.
		"about an unknown incident": {work(t, incident, incidentArgs{}), "about_incident", nil},
		"unscoped":                  {work(t, func(w river.Worker[sweepArgs]) river.Worker[sweepArgs] { return unscopedWorker[sweepArgs]{w} }, sweepArgs{}), "sweep", nil},
	} {
		if tc.line[FieldJob] != tc.job || tc.line[FieldIncidentID] != tc.incident {
			t.Errorf("%s: got %v, want job %s and incident_id %v", name, tc.line, tc.job, tc.incident)
		}
	}
}

// River sees the wrapped worker's other settings.
func TestIncidentWorker_KeepsTheWorkersSettings(t *testing.T) {
	w := incidentWorker[incidentArgs]{timeoutWorker{}}
	if got := w.Timeout(nil); got != 42 {
		t.Errorf("Timeout = %v, want the worker's", got)
	}
}

type timeoutWorker struct {
	logWorker[incidentArgs]
}

func (timeoutWorker) Timeout(*river.Job[incidentArgs]) time.Duration { return 42 }
