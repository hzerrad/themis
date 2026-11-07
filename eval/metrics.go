package eval

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
)

// caseMetrics is what a case's metrics.yaml says the install's metrics source
// reads of each pod's containers. A case without the file has no source.
type caseMetrics struct {
	// Source is prometheus, which knows peaks and throttling, or kubernetes,
	// the metrics API, which knows only the present.
	Source string `json:"source"`
	// Pods holds each container's figures, by pod and container name. A
	// figure left out is one the source has none of.
	Pods map[string]map[string]containerFigures `json:"pods"`
	// Revisions is what Prometheus's history says of each ReplicaSet's
	// pods, by its name. A ReplicaSet left out is one it has none of.
	Revisions map[string]revisionHistory `json:"revisions,omitempty"`
}

// revisionHistory is how one ReplicaSet's pods ran, as Prometheus's history
// shows it: metrics.Record, written by hand.
type revisionHistory struct {
	SeenFrom         time.Time `json:"seenFrom"`
	SeenUntil        time.Time `json:"seenUntil"`
	Restarts         int       `json:"restarts,omitempty"`
	FirstRestartAt   time.Time `json:"firstRestartAt,omitzero"`
	OOMKilled        int       `json:"oomKilled,omitempty"`
	FirstOOMKillAt   time.Time `json:"firstOOMKillAt,omitzero"`
	Throttled        *float64  `json:"throttled,omitempty"`
	FirstThrottledAt time.Time `json:"firstThrottledAt,omitzero"`
	// Use is how much of their limits its containers used, sampled: a
	// span's figure for each container is the average of its samples then.
	Use []useSample `json:"use,omitempty"`
}

// useSample is each container's share of its limits at one time, by
// container name: memory working set over the memory limit, and CPU use
// over the CPU limit, 1 being the limit.
type useSample struct {
	At     time.Time          `json:"at"`
	Memory map[string]float64 `json:"memory,omitempty"`
	CPU    map[string]float64 `json:"cpu,omitempty"`
}

type containerFigures struct {
	// Memory is the working set now, and PeakMemory the most over the
	// window, as quantities such as 200Mi.
	Memory     string `json:"memory,omitempty"`
	PeakMemory string `json:"peakMemory,omitempty"`
	// CPU is what the container uses now, as a quantity such as 450m.
	CPU string `json:"cpu,omitempty"`
	// Throttled is the share of its CPU periods throttled over the window,
	// from 0 to 1.
	Throttled *float64 `json:"throttled,omitempty"`
}

// loadMetrics reads the metrics source the case in dir describes: the real
// metrics API source over its figures, or one answering as Prometheus would.
// It is nil when the case has no metrics.yaml.
func loadMetrics(path string) (metrics.Source, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a path in the repository
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m caseMetrics
	if err := yaml.UnmarshalStrict(data, &m); err != nil {
		return nil, fmt.Errorf("metrics.yaml: %w", err)
	}
	// As an install asks its sources, so the evidence names the one that
	// answered.
	for name, h := range m.Revisions {
		if h.SeenFrom.IsZero() || h.SeenUntil.Before(h.SeenFrom) {
			return nil, fmt.Errorf("metrics.yaml: revisions: %s: seenFrom is required, and seenUntil comes after it", name)
		}
	}
	switch m.Source {
	case metrics.Prometheus:
		return metrics.Sources{promFigures(m)}, nil
	case metrics.Kubernetes:
		if len(m.Revisions) > 0 {
			return nil, errors.New("metrics.yaml: the metrics API keeps no history, so it has no revisions")
		}
		src, err := metricsAPI(m)
		if err != nil {
			return nil, err
		}
		return metrics.Sources{src}, nil
	default:
		return nil, fmt.Errorf("metrics.yaml: source must be %s or %s, got %q", metrics.Prometheus, metrics.Kubernetes, m.Source)
	}
}

// metricsAPI is the real metrics API source, over a cluster serving the
// case's figures. It has no peaks or throttling to serve.
func metricsAPI(m caseMetrics) (metrics.Source, error) {
	list := &metricsv1beta1.PodMetricsList{}
	for _, pod := range slices.Sorted(maps.Keys(m.Pods)) {
		pm := metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: pod}}
		for _, name := range slices.Sorted(maps.Keys(m.Pods[pod])) {
			f := m.Pods[pod][name]
			if f.PeakMemory != "" || f.Throttled != nil {
				return nil, fmt.Errorf("metrics.yaml: %s/%s: the metrics API knows only the present, so it has no peakMemory or throttled", pod, name)
			}
			usage := corev1.ResourceList{}
			for res, s := range map[corev1.ResourceName]string{corev1.ResourceMemory: f.Memory, corev1.ResourceCPU: f.CPU} {
				q, err := resource.ParseQuantity(s)
				if err != nil {
					return nil, fmt.Errorf("metrics.yaml: %s/%s: the metrics API gives both memory and cpu, as quantities: %w", pod, name, err)
				}
				usage[res] = q
			}
			pm.Containers = append(pm.Containers, metricsv1beta1.ContainerMetrics{Name: name, Usage: usage})
		}
		list.Items = append(list.Items, pm)
	}
	c := metricsfake.NewSimpleClientset()
	c.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		out := list.DeepCopy()
		for i := range out.Items {
			out.Items[i].Namespace = a.GetNamespace()
		}
		return true, out, nil
	})
	return metrics.NewKubernetes(c), nil
}

// promFigures answers as Prometheus would: each container's highest figure
// among the pods asked about.
type promFigures caseMetrics

func (promFigures) Name() string { return metrics.Prometheus }

func (p promFigures) Memory(_ context.Context, pods metrics.Pods) (metrics.Reading, error) {
	return p.read(pods, func(f containerFigures) (float64, bool, error) { return bytesIn(f.Memory) })
}

func (p promFigures) PeakMemory(_ context.Context, pods metrics.Pods, _ time.Duration) (metrics.Reading, error) {
	return p.read(pods, func(f containerFigures) (float64, bool, error) { return bytesIn(f.PeakMemory) })
}

func (p promFigures) CPU(_ context.Context, pods metrics.Pods) (metrics.Reading, error) {
	return p.read(pods, func(f containerFigures) (float64, bool, error) { return coresIn(f.CPU) })
}

func (p promFigures) Throttling(_ context.Context, pods metrics.Pods, _ time.Duration) (metrics.Reading, error) {
	return p.read(pods, func(f containerFigures) (float64, bool, error) {
		if f.Throttled == nil {
			return 0, false, nil
		}
		return *f.Throttled, true, nil
	})
}

// History answers from the case's revisions, as Prometheus would from its
// own: a ReplicaSet it has none of is one it can't say anything about.
func (p promFigures) History(_ context.Context, rs metrics.ReplicaSet, _ time.Time) (metrics.Record, error) {
	h, ok := p.Revisions[rs.Name]
	if !ok {
		return metrics.Record{Unavailable: metrics.NotInHistory(rs.Name)}, nil
	}
	return metrics.Record{
		SeenFrom: h.SeenFrom, SeenUntil: h.SeenUntil,
		Restarts: h.Restarts, FirstRestart: h.FirstRestartAt,
		OOMKilled: h.OOMKilled, FirstOOMKill: h.FirstOOMKillAt,
		Throttled: h.Throttled, FirstThrottled: h.FirstThrottledAt,
	}, nil
}

// Use averages each container's samples from from to to, as Prometheus
// averages what it scraped then: a span with no samples is one it has no
// figures of.
func (p promFigures) Use(_ context.Context, rs metrics.ReplicaSet, from, to time.Time) (metrics.Use, error) {
	var then []useSample
	for _, s := range p.Revisions[rs.Name].Use {
		if !s.At.Before(from) && !s.At.After(to) {
			then = append(then, s)
		}
	}
	u := metrics.Use{
		Memory: average(then, func(s useSample) map[string]float64 { return s.Memory }),
		CPU:    average(then, func(s useSample) map[string]float64 { return s.CPU }),
	}
	if len(u.Memory) == 0 && len(u.CPU) == 0 {
		u.Unavailable = "Prometheus has no memory or CPU figures, against a limit, for the pods of ReplicaSet " + rs.Name + " then"
	}
	return u, nil
}

// average is each container's average over the samples of one resource.
func average(samples []useSample, of func(useSample) map[string]float64) map[string]float64 {
	sum, n := map[string]float64{}, map[string]float64{}
	for _, s := range samples {
		for c, v := range of(s) {
			sum[c] += v
			n[c]++
		}
	}
	for c := range sum {
		sum[c] /= n[c]
	}
	return sum
}

func (p promFigures) read(pods metrics.Pods, value func(containerFigures) (float64, bool, error)) (metrics.Reading, error) {
	r := metrics.Reading{Containers: map[string]float64{}}
	for _, pod := range pods.Names {
		for name, f := range p.Pods[pod] {
			v, ok, err := value(f)
			if err != nil {
				return metrics.Reading{}, fmt.Errorf("metrics.yaml: %s/%s: %w", pod, name, err)
			}
			if seen, had := r.Containers[name]; ok && (!had || v > seen) {
				r.Containers[name] = v
			}
		}
	}
	if len(r.Containers) == 0 {
		r.Unavailable = "Prometheus has no such figures for these pods"
	}
	return r, nil
}

// bytesIn is the memory quantity s in bytes; false when s is empty.
func bytesIn(s string) (float64, bool, error) {
	q, ok, err := parsed(s)
	return float64(q.Value()), ok, err
}

// coresIn is the CPU quantity s in cores; false when s is empty.
func coresIn(s string) (float64, bool, error) {
	q, ok, err := parsed(s)
	return float64(q.MilliValue()) / 1000, ok, err
}

func parsed(s string) (resource.Quantity, bool, error) {
	if s == "" {
		return resource.Quantity{}, false, nil
	}
	q, err := resource.ParseQuantity(s)
	return q, err == nil, err
}
