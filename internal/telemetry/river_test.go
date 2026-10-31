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

func TestIncidentWorker_LogsJobAndIncident(t *testing.T) {
	line := work(t, func(w river.Worker[incidentArgs]) river.Worker[incidentArgs] { return incidentWorker[incidentArgs]{w} }, incidentArgs{IncidentID: "inc-1"})
	if line[FieldJob] != "about_incident" || line[FieldIncidentID] != "inc-1" {
		t.Errorf("got %v, want job and incident_id", line)
	}
}

// A job queued before its args carried the incident logs no empty field.
func TestIncidentWorker_OmitsAnUnknownIncident(t *testing.T) {
	line := work(t, func(w river.Worker[incidentArgs]) river.Worker[incidentArgs] { return incidentWorker[incidentArgs]{w} }, incidentArgs{})
	if _, ok := line[FieldIncidentID]; ok {
		t.Errorf("got %v, want no incident_id", line)
	}
}

func TestUnscopedWorker_LogsJob(t *testing.T) {
	line := work(t, func(w river.Worker[sweepArgs]) river.Worker[sweepArgs] { return unscopedWorker[sweepArgs]{w} }, sweepArgs{})
	if line[FieldJob] != "sweep" {
		t.Errorf("got %v, want job", line)
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
