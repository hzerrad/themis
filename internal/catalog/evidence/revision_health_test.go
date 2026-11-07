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

// Once the history has both revisions, it compares how much of their limits
// their containers used over the same time since each started, the shorter
// of the two lives, and the earlier one's over its last that long too.
func TestRevisionHealth_ComparesUseAtTheSameAge(t *testing.T) {
	target := loadScenario1(t)
	w := read(t, target)
	revisions, err := w.Revisions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	created := map[string]time.Time{}
	for _, r := range revisions {
		created[r.Object.Name] = r.Object.CreationTimestamp.UTC()
	}
	curSince := created["checkout-7d9f8b6c4"]
	if w.WrittenAt.After(curSince) {
		curSince = w.WrittenAt
	}
	priorSince := created["checkout-5c6a1e2f3"]
	week := 7 * 24 * time.Hour
	src := &fakeSource{
		records: map[string]metrics.Record{
			"checkout-7d9f8b6c4": {Source: "prometheus", SeenFrom: curSince, SeenUntil: curSince.Add(20 * time.Minute)},
			"checkout-5c6a1e2f3": {Source: "prometheus", SeenFrom: priorSince, SeenUntil: priorSince.Add(week)},
		},
		use: func(rs string, from, _ time.Time) metrics.Use {
			switch {
			case rs == "checkout-7d9f8b6c4":
				return metrics.Use{Memory: map[string]float64{"app": 0.97}}
			case from.Equal(priorSince):
				return metrics.Use{Memory: map[string]float64{"app": 0.45}, CPU: map[string]float64{"app": 0.2}}
			default:
				return metrics.Use{Memory: map[string]float64{"app": 0.47123}}
			}
		},
	}
	item, err := evidence.NewRevisionHealth(src).Collect(t.Context(), k8s, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	var view evidence.RevisionHealthView
	if err := json.Unmarshal(item.Data, &view); err != nil {
		t.Fatal(err)
	}
	at := func(t time.Time) string { return t.Format(time.RFC3339) }
	want := []string{
		"checkout-7d9f8b6c4 " + at(curSince) + " " + at(curSince.Add(20*time.Minute)),
		"checkout-5c6a1e2f3 " + at(priorSince) + " " + at(priorSince.Add(20*time.Minute)),
		"checkout-5c6a1e2f3 " + at(priorSince.Add(week-20*time.Minute)) + " " + at(priorSince.Add(week)),
	}
	if strings.Join(src.spans, "\n") != strings.Join(want, "\n") {
		t.Errorf("asked about\n%s\nwant\n%s", strings.Join(src.spans, "\n"), strings.Join(want, "\n"))
	}
	if view.ComparedAge != "20m" {
		t.Errorf("comparedAge = %q, want the current revision's 20m", view.ComparedAge)
	}
	cur, prior := view.Revisions[0], view.Revisions[1]
	if len(cur.Memory) != 1 || *cur.Memory[0].First != 0.97 || cur.Memory[0].Last != nil || cur.CPU != nil {
		t.Errorf("current = %+v, want app's memory over its first 20m only", cur.Memory)
	}
	if len(prior.Memory) != 1 || *prior.Memory[0].First != 0.45 || *prior.Memory[0].Last != 0.471 ||
		len(prior.CPU) != 1 || *prior.CPU[0].First != 0.2 || prior.CPU[0].Last != nil {
		t.Errorf("before = %+v %+v, want app's memory over its first and last 20m, and its CPU over its first", prior.Memory, prior.CPU)
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
