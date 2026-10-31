package telemetry_test

import (
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

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

func TestTracing_NoEndpoint_NoOp(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	shutdown, err := telemetry.SetupTracing(t.Context(), "core")
	if err != nil {
		t.Fatal(err)
	}
	_, span := telemetry.Tracer().Start(t.Context(), "work")
	defer span.End()
	if span.IsRecording() || span.SpanContext().IsValid() {
		t.Error("without an endpoint, spans are recorded")
	}
	if err := telemetry.StopTracing(shutdown); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestTracing_UnknownProtocolRefused(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "carrier-pigeon")
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	if _, err := telemetry.SetupTracing(t.Context(), "core"); err == nil {
		t.Error("an unknown protocol was accepted")
	}
}

func TestSpans_CarryIncidentID(t *testing.T) {
	rec := record(t)
	ctx, span := telemetry.Tracer().Start(t.Context(), "work")
	telemetry.WithAttrs(ctx, slog.String(telemetry.FieldIncidentID, "inc-1"), slog.String("pod", "not-an-id"))
	span.End()

	attrs := rec.Ended()[0].Attributes()
	if !has(attrs, "themis.incident.id", "inc-1") {
		t.Errorf("attributes = %v, want themis.incident.id", attrs)
	}
	for _, a := range attrs {
		if a.Key == "pod" {
			t.Error("a field that is not an ID reached the span")
		}
	}
}

func has(attrs []attribute.KeyValue, key, value string) bool {
	for _, a := range attrs {
		if string(a.Key) == key && a.Value.AsString() == value {
			return true
		}
	}
	return false
}

// The executor's span continues the trace of the core's call.
func TestGRPC_Call_SharesTrace(t *testing.T) {
	rec := record(t)
	client, _ := serveTraced(t)
	ctx, parent := telemetry.Tracer().Start(t.Context(), "execute")
	if _, err := client.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	parent.End()

	var server, clientSpan sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			server = s
		}
		if s.SpanKind() == trace.SpanKindClient {
			clientSpan = s
		}
	}
	if server == nil || clientSpan == nil {
		t.Fatalf("spans = %v, want a client and a server span", rec.Ended())
	}
	if server.SpanContext().TraceID() != parent.SpanContext().TraceID() || server.Parent().SpanID() != clientSpan.SpanContext().SpanID() {
		t.Error("the server span is not in the caller's trace, under its client span")
	}
}

// The investigator polls for leases; those calls make no server spans.
func TestGRPC_UntracedMethodMakesNoServerSpan(t *testing.T) {
	rec := record(t)
	client, _ := serveTraced(t, "Check")
	if _, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			t.Errorf("an untraced method made server span %q", s.Name())
		}
	}
}
