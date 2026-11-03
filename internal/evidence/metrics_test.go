package evidence_test

import (
	"errors"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

func TestMetrics_EvidenceCollectedTruncatedRedacted(t *testing.T) {
	labels := map[string]string{"template": "fake"}
	collected := metrictest.Delta(t, "themis_evidence_collected_total", labels)
	truncated := metrictest.Delta(t, "themis_evidence_truncated_total", labels)
	redactions := metrictest.Delta(t, "themis_redactions_total", labels)

	r := evidence.NewRegistry(fakeTemplate{item: evidence.Item{Data: []byte(`{"env":"password=a token=b"}`), Truncated: true}})
	if _, err := r.Collect(t.Context(), nil, "fake", fakeWorkload, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	failing := evidence.NewRegistry(fakeTemplate{err: evidence.ErrNoPods})
	if _, err := failing.Collect(t.Context(), nil, "fake", fakeWorkload, nil, time.Now()); !errors.Is(err, evidence.ErrNoPods) {
		t.Errorf("err = %v, want the template's ErrNoPods", err)
	}

	if got := collected(); got != 1 {
		t.Errorf("collected %v, want 1: a failed collection is not evidence", got)
	}
	if got := truncated(); got != 1 {
		t.Errorf("truncated %v, want 1", got)
	}
	if got := redactions(); got != 2 {
		t.Errorf("redactions %v, want 2", got)
	}
}

// Every registered template has its missing-evidence series from the start,
// so a dashboard shows zero for it rather than nothing.
func TestMetrics_MissingEvidenceStartsAtZero(t *testing.T) {
	evidence.NewRegistry(fakeTemplate{})
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "themis_missing_evidence_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["template"] == "fake" {
				seen[labels["cause"]] = true
			}
		}
	}
	for _, c := range evidence.GapCauses() {
		if !seen[string(c)] {
			t.Errorf("no themis_missing_evidence_total series for template fake, cause %s", c)
		}
	}
}
