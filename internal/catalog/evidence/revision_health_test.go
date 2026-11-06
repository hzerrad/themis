package evidence_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
)

// The current revision's record runs from its rollout, the earlier one's from
// its ReplicaSet's creation, each as the source's history says it ran.
func TestRevisionHealth_CurrentAndTheOneBefore(t *testing.T) {
	failed := time.Date(2026, 11, 4, 13, 40, 0, 0, time.UTC)
	share := 0.4
	src := &fakeSource{
		noHistory: "Prometheus has none of them",
		records: map[string]metrics.Record{
			"checkout-7d9f8b6c4": {Source: "prometheus", SeenFrom: failed.Add(-time.Hour), SeenUntil: failed, Restarts: 3, FirstRestart: failed, OOMKilled: 1, FirstOOMKill: failed, Throttled: &share},
		},
	}
	target := loadScenario1(t)
	item, err := evidence.NewRevisionHealth(src).Collect(t.Context(), k8s, read(t, target), nil)
	if err != nil {
		t.Fatal(err)
	}
	var view evidence.RevisionHealthView
	if err := json.Unmarshal(item.Data, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Revisions) != 2 {
		t.Fatalf("revisions = %+v, want the current one and the one before", view.Revisions)
	}
	cur, prior := view.Revisions[0], view.Revisions[1]
	if cur.Revision != "2" || !cur.Current || cur.ReplicaSet != "checkout-7d9f8b6c4" || cur.Restarts != 3 ||
		!cur.FirstOOMKillAt.Equal(failed) || *cur.ThrottledRatio != 0.4 || cur.Source != "prometheus" {
		t.Errorf("current = %+v, want revision 2's record", cur)
	}
	// A revision the history has none of says why, with nothing made up.
	if prior.Revision != "1" || prior.Current || prior.Unavailable != "Prometheus has none of them" || !prior.SeenFrom.IsZero() || prior.ThrottledRatio != nil {
		t.Errorf("before = %+v, want revision 1 with why there is no record", prior)
	}
	for _, r := range view.Revisions {
		if since := src.since[r.ReplicaSet]; since.IsZero() || !since.Equal(r.Since) {
			t.Errorf("%s was read from %v, and says %v", r.ReplicaSet, since, r.Since)
		}
	}
}

// A source with no history of either revision leaves a gap that says why.
func TestRevisionHealth_NoHistory(t *testing.T) {
	target := loadScenario1(t)
	for name, tc := range map[string]struct {
		src  metrics.Source
		says string
	}{
		"no source":            {nil, metrics.NoSource},
		"the metrics API only": {metrics.Sources{&fakeSource{noHistory: metrics.NoRevisionHistory}}, metrics.NoRevisionHistory},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := evidence.NewRevisionHealth(tc.src).Collect(t.Context(), k8s, read(t, target), nil)
			if !errors.Is(err, evidence.ErrNoMetrics) || evidence.CauseOf(err) != evidence.GapUnavailable || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want no figures, saying %q", err, tc.says)
			}
		})
	}
}
