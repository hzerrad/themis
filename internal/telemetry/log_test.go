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

func TestLogger_AddsContextFields(t *testing.T) {
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, "core", slog.LevelInfo)

	ctx := telemetry.WithAttrs(context.Background(), slog.String(telemetry.FieldIncidentID, "inc-1"))
	nested := telemetry.WithAttrs(ctx, slog.String(telemetry.FieldActionID, "act-1"))
	log.InfoContext(ctx, "first")
	log.InfoContext(nested, "second")
	log.InfoContext(ctx, "third")

	got := lines(t, &buf)
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3", len(got))
	}
	for i, l := range got {
		if l[telemetry.FieldIncidentID] != "inc-1" {
			t.Errorf("line %d: incident_id = %v, want inc-1", i, l[telemetry.FieldIncidentID])
		}
	}
	if got[1][telemetry.FieldActionID] != "act-1" {
		t.Errorf("nested line: action_id = %v, want act-1", got[1][telemetry.FieldActionID])
	}
	if _, ok := got[2][telemetry.FieldActionID]; ok {
		t.Error("the nested context's field leaked to its parent")
	}
}

func TestLogger_SiblingContextsDoNotShareFields(t *testing.T) {
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, "core", slog.LevelInfo)

	parent := telemetry.WithAttrs(context.Background(), slog.String(telemetry.FieldIncidentID, "inc-1"))
	a := telemetry.WithAttrs(parent, slog.String(telemetry.FieldActionID, "a"))
	b := telemetry.WithAttrs(parent, slog.String(telemetry.FieldActionID, "b"))
	log.InfoContext(a, "a")
	log.InfoContext(b, "b")

	got := lines(t, &buf)
	if got[0][telemetry.FieldActionID] != "a" || got[1][telemetry.FieldActionID] != "b" {
		t.Errorf("action_id = %v, %v; want a, b", got[0][telemetry.FieldActionID], got[1][telemetry.FieldActionID])
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

func TestLogger_Component_OnEveryLine(t *testing.T) {
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, "executor", slog.LevelInfo)

	log.Info("plain")
	log.InfoContext(context.Background(), "with context")
	log.With("key", "value").Warn("derived")

	for i, l := range lines(t, &buf) {
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

func TestLogger_DropsLinesBelowLevel(t *testing.T) {
	var level slog.Level
	if err := level.UnmarshalText([]byte("info")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, "core", level)

	log.Debug("hidden")
	log.Info("shown")

	got := lines(t, &buf)
	if len(got) != 1 || got[0][slog.MessageKey] != "shown" {
		t.Errorf("got %v, want only the info line", got)
	}
}
