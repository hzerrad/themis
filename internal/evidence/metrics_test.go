package evidence_test

import (
	"errors"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

func TestMetrics_EvidenceCollectedTruncatedRedacted(t *testing.T) {
	labels := map[string]string{"template": "fake"}
	collected := metrictest.Delta(t, "themis_evidence_collected_total", labels)
	truncated := metrictest.Delta(t, "themis_evidence_truncated_total", labels)
	redactions := metrictest.Delta(t, "themis_redactions_total", labels)

	r := evidence.NewRegistry(fakeTemplate{item: evidence.Item{Data: []byte(`{"env":"password=a token=b"}`), Truncated: true}})
	if _, err := r.Collect(t.Context(), nil, "fake", target, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	failing := evidence.NewRegistry(fakeTemplate{err: errors.New("unreachable")})
	_, _ = failing.Collect(t.Context(), nil, "fake", target, nil, time.Now())

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
