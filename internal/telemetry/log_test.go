package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/hzerrad/themis/internal/telemetry"
)

// lines decodes each JSON line logged to buf.
func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

// A context's fields reach its children, never its parent or its siblings.
func TestLogger_AddsContextFields(t *testing.T) {
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, "core", slog.LevelInfo)

	ctx := telemetry.WithAttrs(context.Background(), slog.String(telemetry.FieldIncidentID, "inc-1"))
	nested := telemetry.WithAttrs(ctx, slog.String(telemetry.FieldActionID, "act-1"))
	sibling := telemetry.WithAttrs(ctx, slog.String(telemetry.FieldActionID, "act-2"))
	log.InfoContext(ctx, "first")
	log.InfoContext(nested, "second")
	log.InfoContext(ctx, "third")
	log.InfoContext(sibling, "fourth")

	got := lines(t, &buf)
	if len(got) != 4 {
		t.Fatalf("got %d lines, want 4", len(got))
	}
	for i, l := range got {
		if l[telemetry.FieldIncidentID] != "inc-1" {
			t.Errorf("line %d: incident_id = %v, want inc-1", i, l[telemetry.FieldIncidentID])
		}
	}
	if got[1][telemetry.FieldActionID] != "act-1" || got[3][telemetry.FieldActionID] != "act-2" {
		t.Errorf("action_id = %v, %v; want act-1, act-2", got[1][telemetry.FieldActionID], got[3][telemetry.FieldActionID])
	}
	if _, ok := got[2][telemetry.FieldActionID]; ok {
		t.Error("the nested context's field leaked to its parent")
	}
}

// A field set again is logged once, with its latest value.
func TestLogger_LaterFieldReplacesEarlier(t *testing.T) {
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, "core", slog.LevelInfo)

	ctx := telemetry.WithAttrs(context.Background(), slog.String(telemetry.FieldIncidentID, "from-caller"))
	log.InfoContext(telemetry.WithAttrs(ctx, slog.String(telemetry.FieldIncidentID, "from-record")), "replaced")

	if n := strings.Count(buf.String(), `"incident_id"`); n != 1 {
		t.Fatalf("incident_id logged %d times: %s", n, buf.String())
	}
	if got := lines(t, &buf)[0][telemetry.FieldIncidentID]; got != "from-record" {
		t.Errorf("incident_id = %v, want from-record", got)
	}
}

func TestLogger_AddsTraceIDs_OnlyWithActiveSpan(t *testing.T) {
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, "core", slog.LevelInfo)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	log.InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "traced")
	log.InfoContext(context.Background(), "untraced")

	got := lines(t, &buf)
	if got[0][telemetry.FieldTraceID] != sc.TraceID().String() || got[0][telemetry.FieldSpanID] != sc.SpanID().String() {
		t.Errorf("traced line: trace_id = %v, span_id = %v", got[0][telemetry.FieldTraceID], got[0][telemetry.FieldSpanID])
	}
	for _, f := range []string{telemetry.FieldTraceID, telemetry.FieldSpanID} {
		if _, ok := got[1][f]; ok {
			t.Errorf("untraced line has %s", f)
		}
	}
}

// Every line names its component, and lines below the level are dropped.
func TestLogger_Component_OnEveryLine(t *testing.T) {
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, "executor", slog.LevelInfo)

	log.Debug("hidden")
	log.Info("plain")
	log.InfoContext(context.Background(), "with context")
	log.With("key", "value").Warn("derived")

	got := lines(t, &buf)
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3 without the debug line", len(got))
	}
	for i, l := range got {
		if l[telemetry.FieldComponent] != "executor" {
			t.Errorf("line %d: component = %v, want executor", i, l[telemetry.FieldComponent])
		}
	}
}

func TestLogger_With_KeepsContextFields(t *testing.T) {
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, "core", slog.LevelInfo).With("key", "value")

	log.InfoContext(telemetry.WithAttrs(context.Background(), slog.String(telemetry.FieldIncidentID, "inc-1")), "derived")

	got := lines(t, &buf)[0]
	if got[telemetry.FieldIncidentID] != "inc-1" || got["key"] != "value" {
		t.Errorf("got %v, want incident_id and key", got)
	}
}
