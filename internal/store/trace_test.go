package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
)

// record installs a provider that keeps every span in memory.
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
