package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
)

// record installs a provider that keeps every span in memory. Call it before
// newStore, whose job middleware takes the provider then.
func record(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func TestTransition_RecordsTraceID(t *testing.T) {
	record(t)
	s, _ := newStore(t)
	id, _ := createOrJoin(t, s, newIncident("fp-traced"))
	ctx, span := telemetry.Tracer().Start(t.Context(), "work")
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		return s.Transition(ctx, tx, store.TransitionArgs{ID: id, From: incident.Received, To: incident.Investigating, Reason: "test", Actor: "test"})
	})
	span.End()
	if err != nil {
		t.Fatal(err)
	}
	advance(t, s, id, incident.Investigating, incident.Escalated)

	trail, err := s.Transitions(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if trail[0].TraceID != span.SpanContext().TraceID().String() {
		t.Errorf("traced transition: trace ID = %q, want %s", trail[0].TraceID, span.SpanContext().TraceID())
	}
	if trail[1].TraceID != "" {
		t.Errorf("untraced transition: trace ID = %q, want none", trail[1].TraceID)
	}
}

// Tracing is transport: it never changes a job's args, which define the job.
func TestJob_TraceContextInMetadataNotArgs(t *testing.T) {
	record(t)
	s, pool := newStore(t)
	enqueue := func(ctx context.Context) {
		t.Helper()
		if err := s.InTx(ctx, func(tx pgx.Tx) error { return s.EnqueueTx(ctx, tx, tracedJobArgs{Incident: "inc-1"}) }); err != nil {
			t.Fatal(err)
		}
	}
	ctx, span := telemetry.Tracer().Start(t.Context(), "enqueue")
	enqueue(ctx)
	span.End()
	enqueue(t.Context())

	rows, err := pool.Query(t.Context(), `SELECT args::text, metadata->>'traceparent' FROM river_job WHERE kind = 'traced' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type job struct{ args, traceparent string }
	jobs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (job, error) {
		var j job
		var tp *string
		err := r.Scan(&j.args, &tp)
		if tp != nil {
			j.traceparent = *tp
		}
		return j, err
	})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %v, %v", jobs, err)
	}
	if jobs[0].args != jobs[1].args {
		t.Errorf("args differ with a span: %s and %s", jobs[0].args, jobs[1].args)
	}
	if want := span.SpanContext().TraceID().String(); len(jobs[0].traceparent) < 35 || jobs[0].traceparent[3:35] != want {
		t.Errorf("traceparent = %q, want one in trace %s", jobs[0].traceparent, want)
	}
	// Queued outside any span, a job links to the insert's own trace.
	if strings.Contains(jobs[1].traceparent, span.SpanContext().TraceID().String()) {
		t.Errorf("a job queued outside the span has its trace: %q", jobs[1].traceparent)
	}
}

type tracedJobArgs struct {
	Incident string `json:"incident"`
}

func (tracedJobArgs) Kind() string { return "traced" }

type tracedWorker struct {
	river.WorkerDefaults[tracedJobArgs]
	done chan struct{}
}

func (w *tracedWorker) Work(context.Context, *river.Job[tracedJobArgs]) error {
	close(w.done)
	return nil
}

// A job runs in a trace of its own, linked to the span that queued it: it may
// run long after that trace ended.
func TestJob_LinksToEnqueuingSpan(t *testing.T) {
	rec := record(t)
	s, _ := newStore(t)
	w := &tracedWorker{done: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	runner, err := s.NewWorkerRunner(workers, map[string]int{river.QueueDefault: 1}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, enqueuing := telemetry.Tracer().Start(t.Context(), "enqueue")
	if err := s.InTx(ctx, func(tx pgx.Tx) error { return s.EnqueueTx(ctx, tx, tracedJobArgs{}) }); err != nil {
		t.Fatal(err)
	}
	enqueuing.End()

	runCtx, stop := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- runner.Start(runCtx) }()
	select {
	case <-w.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the job did not run")
	}
	stop()
	if err := <-stopped; err != nil {
		t.Fatalf("runner: %v", err)
	}

	for _, span := range rec.Ended() {
		if span.Name() != "river.work/traced" {
			continue
		}
		if span.SpanContext().TraceID() == enqueuing.SpanContext().TraceID() {
			t.Error("the job ran in the trace that queued it")
		}
		for _, l := range span.Links() {
			if l.SpanContext.TraceID() == enqueuing.SpanContext().TraceID() {
				return
			}
		}
		t.Fatalf("links = %v, want one to the enqueuing trace", span.Links())
	}
	t.Fatal("no work span was recorded")
}
