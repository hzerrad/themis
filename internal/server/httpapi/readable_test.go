package httpapi

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/evidence"
)

// Each kind of evidence is digested in one line and laid out by its kind,
// with the figures people check first; one Themis has no view for shows its
// fields.
func TestShown(t *testing.T) {
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := uiShop(t, cat)
	digests := map[string]string{
		evShopRollout: "1 revision. Created 2026-11-05T21:37:00Z.",
		evShopPods:    "2 pods, 1 ready, 3 restarts. Last ended OOMKilled, exit 137, at 2026-11-06T08:57:00Z.",
		evShopEvents:  "6 events. BackOff ×4, Started ×2.",
		evShopLogs:    "app, previous run. 9 lines. Last: catalog: token=[REDACTED] reloading the feed",
		evShopMemory:  "Peak 57Mi of 60Mi (95%) over 1h, from Prometheus.",
		evShopCPU:     "Uses 120m of 200m (60%). Throttled in 8% of periods over 1h, from Prometheus.",
		evShopConfig:  "1 ConfigMap, 1 Secret. catalog-feed last changed at 2026-11-06T08:30:00Z.",
		evShopHealth:  "Revision 1: 3 OOM kills, first at 2026-11-06T08:53:00Z.",
		evShopNodes:   "5 fields.",
		evShopNow:     "app, current run. 1 line. Last: catalog: loading 64Mi from the feed",
	}
	views := map[string]*readable{}
	for _, it := range items {
		digest, view := shown(it, items)
		if want := digests[it.ID]; digest.String() != want {
			t.Errorf("%s: digest = %q, want %q", it.Template, digest.String(), want)
		}
		views[it.ID] = view
	}

	if rows := views[evShopPods].Pods.Rows; len(rows) != 2 || rows[0].Waiting != "CrashLoopBackOff" || rows[0].Restarts != 2 ||
		rows[0].Ended != "OOMKilled, exit 137" || !rows[0].Failed || !rows[1].Ready {
		t.Errorf("pod rows = %+v", rows)
	}
	if rows := views[evShopEvents].Events.Rows; len(rows) != 2 || !rows[0].Warning || rows[0].Count != 4 || rows[1].Warning {
		t.Errorf("event rows = %+v", rows)
	}

	logs := views[evShopLogs].Logs
	if len(logs.Runs) != 2 || !logs.Runs[0].Previous || logs.Runs[0].N != 4 || logs.Runs[1].Previous || logs.Runs[1].N != 10 {
		t.Fatalf("runs = %+v", logs.Runs)
	}
	first := logs.Runs[0]
	if len(first.Lines) != 9 || !first.Lines[0].At.Equal(uiAt(-7)) || first.Lines[0].Text != "catalog: 0Mi of 64Mi loaded" {
		t.Errorf("lines = %+v", first.Lines)
	}
	if first.Ended != "OOMKilled, exit 137" || !first.EndedAt.Equal(uiAt(-3)) {
		t.Errorf("the previous run ended %q at %s, want how the pod states say", first.Ended, first.EndedAt)
	}
	if notes := views[evShopLogs].Notes; len(notes) != 2 || notes[1].String() != "1 value that looks like a secret is hidden." {
		t.Errorf("log notes = %v", notes)
	}

	// The memory gauge labels itself: the peak at the fill's end, the
	// request at its tick, the limit at the track's end.
	if g := views[evShopMemory].Gauges; len(g) != 1 || g[0].Fill != 95 || g[0].FillText != "peak 57Mi" ||
		g[0].Tick == nil || g[0].Tick.At != 53.3 || g[0].Tick.Text != "request 32Mi" || g[0].End != "limit 60Mi" ||
		g[0].Aria != "app: peak 57Mi of a 60Mi limit, request 32Mi" {
		t.Errorf("memory gauge = %+v", g)
	}
	// Throttling is a figure of its own.
	if g := views[evShopCPU].Gauges; len(g) != 2 || g[0].FillText != "now 120m" || g[0].End != "limit 200m" ||
		g[1].FillText != "throttled 8%" || g[1].Fill != 8 || g[1].End != "100% of periods over 1h" {
		t.Errorf("CPU gauges = %+v", g)
	}
	if r := views[evShopRollout].Revisions; len(r) != 1 || !r[0].First || r[0].Runs[0].String() != "Image busybox:1.37. Memory limit 60Mi, request 32Mi." {
		t.Errorf("revisions = %+v", r)
	}
	if c := views[evShopConfig].Config; len(c) != 2 || c[0].Name != "catalog-feed" || c[1].Kind != "Secret" {
		t.Errorf("config = %+v", c)
	}
	if h := views[evShopHealth].Health; len(h) != 1 || !strings.Contains(h[0].String(), "3 OOM kills, first at") {
		t.Errorf("health = %v", h)
	}
	if f := views[evShopNodes].Fields; len(f) != 5 || f[0] != (fieldRow{"Allocatable", "1 field"}) || f[1] != (fieldRow{"Kubelet", "—"}) ||
		f[2] != (fieldRow{"Memory pressure", "No"}) || f[3] != (fieldRow{"Node", "themis-dev-worker"}) || f[4] != (fieldRow{"Taints", "0 items"}) {
		t.Errorf("fields = %+v", f)
	}
}

// A revision says what changed from the one before it: images, memory and
// CPU, and probe timing.
func TestRevisionChanges(t *testing.T) {
	items := uiEvidence(t, true)
	digest, view := shown(items[0], items)
	if want := "2 revisions. Revision 2 replaced 1 at 2026-11-06T08:40:00Z. The image of container app changed from shop/checkout:1.4.1 to shop/checkout:1.5.0. 2 other changes."; digest.String() != want {
		t.Errorf("digest = %q, want %q", digest.String(), want)
	}
	var changed []string
	for _, c := range view.Revisions[0].Changed {
		changed = append(changed, c.String())
	}
	want := []string{
		"The image of container app changed from shop/checkout:1.4.1 to shop/checkout:1.5.0.",
		"The memory limit of container app changed from 512Mi to 256Mi.",
		"The liveness probe of container app changed.",
	}
	if strings.Join(changed, "\n") != strings.Join(want, "\n") || view.Revisions[0].From != "1" {
		t.Errorf("changed from %q =\n%s\nwant\n%s", view.Revisions[0].From, strings.Join(changed, "\n"), strings.Join(want, "\n"))
	}
	if r := view.Revisions[1]; !r.First || len(r.Changed) > 0 {
		t.Errorf("the first revision = %+v", r)
	}

	// A restart changes only an annotation: said quietly, in place of what
	// the pods run.
	var history evidence.RolloutHistoryView
	if err := json.Unmarshal(items[0].Data, &history); err != nil {
		t.Fatal(err)
	}
	history.Change = &evidence.TemplateChangeView{From: "1", To: "2", App: []string{}, Metadata: []string{"the annotation kubectl.kubernetes.io/restartedAt changed"}}
	restart := items[0]
	data, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	restart.Data = data
	digest, view = shown(restart, items)
	if r := view.Revisions[0]; r.Quiet != "Only annotations changed." || len(r.Changed) > 0 || !strings.HasSuffix(digest.String(), "at 2026-11-06T08:40:00Z. Only annotations changed.") {
		t.Errorf("a restart = %q, %+v", digest.String(), r)
	}

	// The log line written to steer Themis is marked.
	_, logs := shown(items[3], items)
	lines := logs.Logs.Runs[0].Lines
	if last := lines[len(lines)-1]; !last.Flagged || lines[0].Flagged {
		t.Errorf("lines = %+v", lines)
	}
}

// A piece of evidence that isn't what its template says shows its fields,
// and data that isn't JSON says so.
func TestShownFallsBack(t *testing.T) {
	broken := evidence.Item{ID: "x", Template: evidence.PodStates, Data: []byte(`{"pods": "not a list"}`)}
	digest, view := shown(broken, []evidence.Item{broken})
	if digest.String() != "1 field." || len(view.Fields) != 1 || view.Fields[0] != (fieldRow{"Pods", "not a list"}) || view.Pods != nil {
		t.Errorf("a broken item = %q %+v", digest.String(), view)
	}
	text := evidence.Item{ID: "y", Template: "text", Data: []byte(`plain words`)}
	if digest, view := shown(text, nil); digest.String() != "Not a JSON object." || len(view.Fields) != 1 {
		t.Errorf("text = %q %+v", digest.String(), view)
	}
}

// Quantities read as Kubernetes writes them.
func TestQuantities(t *testing.T) {
	for b, want := range map[int64]string{57 << 20: "57Mi", 3 << 29: "1.5Gi", 512: "512", 60<<20 + 300: "60Mi"} {
		if got := mebibytes(b); got != want {
			t.Errorf("mebibytes(%d) = %q, want %q", b, got, want)
		}
	}
	for c, want := range map[float64]string{0.25: "250m", 2: "2", 0.12: "120m"} {
		if got := millicores(c); got != want {
			t.Errorf("millicores(%v) = %q, want %q", c, got, want)
		}
	}
	at, rest := lineTime("2026-11-06 21:14:02,120 catalog: loading")
	if !at.Equal(time.Date(2026, 11, 6, 21, 14, 2, 120e6, time.UTC)) || rest != "catalog: loading" {
		t.Errorf("lineTime = %s %q", at, rest)
	}
	if _, err := json.Marshal(phrase{}.text("x").value("y")); err != nil {
		t.Fatal(err)
	}
}
