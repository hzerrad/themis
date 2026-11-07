package evidence_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/incident"
)

// fakeSource answers with fixed readings, and records what it was asked.
type fakeSource struct {
	memory, peak, cpu, throttling metrics.Reading
	records                       map[string]metrics.Record
	noHistory                     string
	err                           error
	since                         map[string]time.Time

	asked   metrics.Pods
	windows []time.Duration

	// use answers Use; spans records each span Use was asked about.
	use   func(rs string, from, to time.Time) metrics.Use
	spans []string
}

// Use answers from use, or that it has no figures, and records the span.
func (f *fakeSource) Use(_ context.Context, rs metrics.ReplicaSet, from, to time.Time) (metrics.Use, error) {
	f.spans = append(f.spans, rs.Name+" "+from.Format(time.RFC3339)+" "+to.Format(time.RFC3339))
	if f.use == nil {
		return metrics.Use{Unavailable: f.noHistory}, f.err
	}
	return f.use(rs.Name, from, to), f.err
}

func (f *fakeSource) Name() string { return "fake" }

func (f *fakeSource) answer(pods metrics.Pods, window time.Duration, r metrics.Reading) (metrics.Reading, error) {
	f.asked = pods
	if window > 0 {
		f.windows = append(f.windows, window)
	}
	return r, f.err
}

func (f *fakeSource) Memory(_ context.Context, p metrics.Pods) (metrics.Reading, error) {
	return f.answer(p, 0, f.memory)
}

func (f *fakeSource) PeakMemory(_ context.Context, p metrics.Pods, w time.Duration) (metrics.Reading, error) {
	return f.answer(p, w, f.peak)
}

func (f *fakeSource) CPU(_ context.Context, p metrics.Pods) (metrics.Reading, error) {
	return f.answer(p, 0, f.cpu)
}

func (f *fakeSource) Throttling(_ context.Context, p metrics.Pods, w time.Duration) (metrics.Reading, error) {
	return f.answer(p, w, f.throttling)
}

// History answers each ReplicaSet's record, or that it has none of it, and
// records where each was read from.
func (f *fakeSource) History(_ context.Context, rs metrics.ReplicaSet, since time.Time) (metrics.Record, error) {
	if f.since == nil {
		f.since = map[string]time.Time{}
	}
	f.since[rs.Name] = since
	r, ok := f.records[rs.Name]
	if !ok {
		r = metrics.Record{Unavailable: f.noHistory}
	}
	return r, f.err
}

const mib = 1 << 20

// Scenario 1's current revision runs one pod, whose container has a 64Mi
// memory limit and no CPU limit.
const currentPod = "checkout-7d9f8b6c4-x2kqp"

func collectUsage(t *testing.T, tmpl evidence.Template, target incident.TargetRef, params map[string]string, view any) {
	t.Helper()
	item, err := tmpl.Collect(t.Context(), k8s, read(t, target), params)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := json.Unmarshal(item.Data, view); err != nil {
		t.Fatalf("decode %s: %v", item.Data, err)
	}
}

func TestContainerMemory_AgainstTheLimit(t *testing.T) {
	src := &fakeSource{
		memory: metrics.Reading{Source: "kubernetes", Containers: map[string]float64{"app": 48 * mib}},
		peak:   metrics.Reading{Source: "kubernetes", Unavailable: "no history"},
	}
	var view evidence.ContainerMemoryView
	collectUsage(t, evidence.NewContainerMemory(src), loadScenario1(t), nil, &view)

	if !slices.Equal(src.asked.Names, []string{currentPod}) || !slices.Equal(src.windows, []time.Duration{time.Hour}) {
		t.Errorf("asked about %v over %v, want only the current revision's pod over 1h", src.asked.Names, src.windows)
	}
	if view.Window != "1h" || view.Usage.Source != "kubernetes" || view.Peak.Unavailable != "no history" {
		t.Errorf("view = %+v, want the usage's source and why the peak is missing", view)
	}
	if len(view.Containers) != 1 {
		t.Fatalf("containers = %+v, want app", view.Containers)
	}
	c := view.Containers[0]
	if c.Name != "app" || *c.LimitBytes != 64*mib || c.RequestBytes != nil || *c.UsageBytes != 48*mib || *c.UsageOfLimit != 0.75 {
		t.Errorf("app = %+v, want 48Mi of a 64Mi limit, 0.75 of it", c)
	}
	// A figure the source couldn't give is null, never a guess.
	if c.PeakBytes != nil || c.PeakOfLimit != nil {
		t.Errorf("peak = %v, %v, want null", c.PeakBytes, c.PeakOfLimit)
	}
}

func TestContainerCPU_ThrottlingOverTheWindow(t *testing.T) {
	src := &fakeSource{
		cpu:        metrics.Reading{Source: "prometheus", Containers: map[string]float64{"app": 0.2504}},
		throttling: metrics.Reading{Source: "prometheus", Containers: map[string]float64{"app": 0.4}},
	}
	var view evidence.ContainerCPUView
	collectUsage(t, evidence.NewContainerCPU(src), loadScenario1(t), map[string]string{"window": "90m"}, &view)

	if view.Window != "1h30m" || !slices.Equal(src.windows, []time.Duration{90 * time.Minute}) {
		t.Errorf("window %q, asked over %v, want 1h30m", view.Window, src.windows)
	}
	c := view.Containers[0]
	// Without a CPU limit there is nothing to be a share of.
	if *c.UsageCores != 0.25 || *c.ThrottledRatio != 0.4 || c.LimitCores != nil || c.UsageOfLimit != nil {
		t.Errorf("app = %+v, want 0.25 cores, 0.4 throttled, no limit", c)
	}
}

// What a source can't give is a gap with its cause, not a guess.
func TestContainerUsage_Gaps(t *testing.T) {
	target := loadScenario1(t)
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}, "", errors.New("no"))
	for name, tc := range map[string]struct {
		tmpl   evidence.Template
		params map[string]string
		target incident.TargetRef
		want   error
		cause  evidence.GapCause
		says   string
	}{
		"no source": {
			tmpl: evidence.NewContainerMemory(nil), want: evidence.ErrNoMetrics, cause: evidence.GapUnavailable, says: metrics.NoSource,
		},
		"the source refuses": {
			tmpl: evidence.NewContainerCPU(&fakeSource{err: forbidden}), cause: evidence.GapForbidden,
		},
		"a window too long": {
			tmpl: evidence.NewContainerCPU(&fakeSource{}), params: map[string]string{"window": "200h"}, want: evidence.ErrInvalidParams, cause: evidence.GapUnavailable,
		},
		"an unknown parameter": {
			tmpl: evidence.NewContainerMemory(&fakeSource{}), params: map[string]string{"pod": "x"}, want: evidence.ErrInvalidParams, cause: evidence.GapUnavailable,
		},
		"no pods": {
			tmpl: evidence.NewContainerMemory(&fakeSource{}), target: podless(t), want: evidence.ErrNoPods, cause: evidence.GapAbsent,
		},
	} {
		t.Run(name, func(t *testing.T) {
			tgt := target
			if tc.target.UID != "" {
				tgt = tc.target
			}
			_, err := tc.tmpl.Collect(t.Context(), k8s, read(t, tgt), tc.params)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || evidence.CauseOf(err) != tc.cause || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v (%s), want %v, %s, saying %q", err, evidence.CauseOf(err), tc.want, tc.cause, tc.says)
			}
		})
	}
}

func podless(t *testing.T) incident.TargetRef {
	t.Helper()
	ns := k8s.NewNamespace(t)
	d := k8s.NewDeployment(t, ns, "idle")
	return incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: d.Name, UID: string(d.UID)}
}

// History names the source of the figures over a window, as people know it,
// and only when one gave them.
func TestHistory(t *testing.T) {
	item := func(template string, view any) evidence.Item {
		data, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		return evidence.Item{Template: template, Data: data}
	}
	for name, tc := range map[string]struct {
		it   evidence.Item
		want string
	}{
		"throttling from Prometheus": {item(evidence.ContainerCPU, evidence.ContainerCPUView{Window: "1h", Throttling: evidence.ReadingView{Source: "prometheus"}}), "Prometheus's container CPU history over 1h"},
		"peaks from Prometheus":      {item(evidence.ContainerMemory, evidence.ContainerMemoryView{Window: "15m", Peak: evidence.ReadingView{Source: "prometheus"}}), "Prometheus's container memory history over 15m"},
		"no peaks":                   {item(evidence.ContainerMemory, evidence.ContainerMemoryView{Window: "15m", Peak: evidence.ReadingView{Unavailable: "no metrics source is set up"}}), ""},
		"no window to its figures":   {item(evidence.PodStates, evidence.PodStatesView{}), ""},
	} {
		if got := tc.it.History(); got != tc.want {
			t.Errorf("%s: History = %q, want %q", name, got, tc.want)
		}
	}
}
