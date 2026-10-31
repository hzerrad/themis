package store_test

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

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
