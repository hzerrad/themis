package httpapi

import (
	"slices"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// The page opens with what the checks found that matters most, as the packs'
// checks write it: the failure first, then what decides the fix, each citing
// its evidence by number, at most four, the same sentence once, and never a
// check that only says whether figures are there. With no fix proposed, the
// title is what fails. Viewers read the facts too: they come from the
// checks, which they may read, not from raw evidence.
func TestFacts(t *testing.T) {
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &API{CheckList: cat.Checks, Fixes: cat.Fixes, Config: config.Config{Profiles: cat.Profiles}}
	items, results := uiShop(t, cat)
	rec := incidentRecord{
		incident: store.Incident{ID: uiID(27), State: incident.AwaitingSuggestion, Profile: "oom-limit-too-low", Target: shopTarget, CreatedAt: uiAt(0)},
		evidence: items, predicates: results,
	}

	d := api.viewDetail(rec, true)
	want := []struct {
		text  string
		cites []citeView
	}{
		{"The app container of revision 1 was OOMKilled in both its pods, last at 2026-11-06T08:57:00Z.", []citeView{{2, "Pod states"}}},
		{"Revision 1 is the first revision, so there is no earlier version to roll back to.", []citeView{{1, "Rollout history"}}},
		{"The memory of app peaked at 57Mi of its 60Mi limit over 1h.", []citeView{{5, "Container memory"}}},
		{"No pod was evicted for its node's memory.", []citeView{{3, "Events"}}},
	}
	if len(d.Facts) != len(want) {
		t.Fatalf("facts = %v, want %d", d.Facts, len(want))
	}
	for i, w := range want {
		if got := d.Facts[i].Words.String(); got != w.text {
			t.Errorf("fact %d = %q, want %q", i+1, got, w.text)
		}
		if !slices.Equal(d.Facts[i].Cites, w.cites) {
			t.Errorf("fact %d cites %v, want %v", i+1, d.Facts[i].Cites, w.cites)
		}
	}
	// A literal value and a moment are pieces of their own.
	if p := d.Facts[0].Words; p[1].Value != "app" || !p[3].Time.Equal(uiAt(-3)) {
		t.Errorf("pieces of the first fact = %#v", p)
	}
	if got := d.Situation.String(); got != "catalog runs out of memory." {
		t.Errorf("situation = %q", got)
	}
	if !slices.Contains(d.Symptoms, "oom_killed_current_revision") {
		t.Errorf("symptoms = %v", d.Symptoms)
	}

	plain := api.viewDetail(rec, false)
	if len(plain.Facts) != len(want) {
		t.Errorf("a viewer reads %d facts, want %d", len(plain.Facts), len(want))
	}
	for _, e := range plain.Evidence {
		if e.Digest != nil || e.View != nil || e.Data != nil {
			t.Errorf("a viewer reads what %s holds", e.Kind)
		}
	}
}

// Facts follow what matters most, whatever order the checks ran in: a
// symptom found leads, a check that only says whether figures are there is
// left out, and a sentence two checks write shows once.
func TestFactsOrder(t *testing.T) {
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &API{CheckList: cat.Checks, Config: config.Config{Profiles: cat.Profiles}}
	in := store.Incident{Profile: "oom-limit-too-low"}
	r := func(name string, v check.Truth, fact string) check.Result {
		return check.Result{Name: name, Value: v, Fact: fact, EvidenceIDs: []string{"e1"}}
	}
	results := []check.Result{
		r("memory_data_available", check.True, "Prometheus says it."),
		r("evicted_for_node_memory", check.False, "No pod was evicted."),
		r("earlier_revision_healthy", check.False, "No earlier version."),
		r("failures_came_with_rollout", check.False, "No earlier version."),
		r("oom_killed_current_revision", check.True, "app was OOMKilled."),
	}
	got := api.facts(in, results, func([]string) []citeView { return nil })
	var texts []string
	for _, f := range got {
		texts = append(texts, f.Words.String())
	}
	if want := []string{"app was OOMKilled.", "No earlier version.", "No pod was evicted."}; !slices.Equal(texts, want) {
		t.Errorf("facts = %q, want %q", texts, want)
	}
}

// A text a check writes is read in pieces: values in backticks, and moments
// in RFC 3339.
func TestWritten(t *testing.T) {
	at := time.Date(2026, 11, 6, 21, 17, 41, 0, time.UTC)
	got := written("The `app` container was OOMKilled at 2026-11-06T21:17:41Z, `twice`.")
	want := phrase{{Text: "The "}, {Value: "app"}, {Text: " container was OOMKilled at "}, {Time: at}, {Text: ", "}, {Value: "twice"}, {Text: "."}}
	if !slices.Equal(got, want) {
		t.Errorf("written = %#v, want %#v", got, want)
	}
}
