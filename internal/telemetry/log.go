// Package telemetry holds what every Themis component shares to make its
// logs, metrics and traces correlate.
package telemetry

import (
	"context"
	"io"
	"log/slog"
	"slices"

	"go.opentelemetry.io/otel/trace"
)

// Field names shared by every component's logs.
const (
	FieldComponent       = "component"
	FieldIncidentID      = "incident_id"
	FieldOccurrenceID    = "occurrence_id"
	FieldInvestigationID = "investigation_id"
	FieldLeaseID         = "lease_id"
	FieldProposalID      = "proposal_id"
	FieldActionID        = "action_id"
	FieldJob             = "job"
	FieldTraceID         = "trace_id"
	FieldSpanID          = "span_id"
	FieldError           = "error"
)

type attrsKey struct{}

// WithAttrs returns a context whose log lines carry attrs, after those of ctx.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	prev, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	return context.WithValue(ctx, attrsKey{}, append(slices.Clip(prev), attrs...))
}

// NewLogger returns a JSON logger for component. Its *Context methods add the
// context's attributes and, when a span is active, its trace and span IDs.
func NewLogger(w io.Writer, component string, level slog.Leveler) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(handler{h}).With(FieldComponent, component)
}

type handler struct{ slog.Handler }

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(attrsKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String(FieldTraceID, sc.TraceID().String()), slog.String(FieldSpanID, sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return handler{h.Handler.WithAttrs(attrs)}
}

func (h handler) WithGroup(name string) slog.Handler {
	return handler{h.Handler.WithGroup(name)}
}
