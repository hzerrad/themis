package telemetry

import (
	"context"
	"errors"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName names the spans Themis starts itself.
const TracerName = "github.com/hzerrad/themis"

// SetupTracing exports spans of component over OTLP when an OTEL_EXPORTER_OTLP
// endpoint is set, and does nothing otherwise: no exporter, no network calls.
// The OTEL_* variables configure the rest, sampling included.
func SetupTracing(ctx context.Context, component string) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := newExporter(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName("themis-"+component)))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// shutdownTimeout bounds flushing the spans left at exit.
const shutdownTimeout = 5 * time.Second

// StopTracing flushes the spans left, within a bound.
func StopTracing(shutdown func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return shutdown(ctx)
}

// newExporter speaks the protocol OTEL_EXPORTER_OTLP_PROTOCOL names, gRPC by
// default.
func newExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch protocol {
	case "", "grpc":
		return otlptracegrpc.New(ctx)
	case "http/protobuf":
		return otlptracehttp.New(ctx)
	default:
		return nil, errors.New("OTEL_EXPORTER_OTLP_PROTOCOL must be grpc or http/protobuf")
	}
}

// Tracer starts the spans Themis names itself.
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// spanAttributes name, on spans, the log fields that identify an object.
var spanAttributes = map[string]string{
	FieldIncidentID:   "themis.incident.id",
	FieldOccurrenceID: "themis.occurrence.id",
	FieldLeaseID:      "themis.lease.id",
	FieldProposalID:   "themis.proposal.id",
	FieldActionID:     "themis.action.id",
	FieldJob:          "themis.job.kind",
}
