// Package metrictest reads metrics registered with controller-runtime's
// registry, which Themis serves on /metrics. Metrics are process-wide, so tests
// compare a value before and after the action they test.
package metrictest

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Value returns the value of the counter or gauge name with exactly labels, or
// 0 if that series does not exist yet.
func Value(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !matches(m.GetLabel(), labels) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	return 0
}

type labelPair interface {
	GetName() string
	GetValue() string
}

func matches[L labelPair](pairs []L, want map[string]string) bool {
	if len(pairs) != len(want) {
		return false
	}
	for _, p := range pairs {
		if v, ok := want[p.GetName()]; !ok || v != p.GetValue() {
			return false
		}
	}
	return true
}

// Delta returns a function that reports how much the series changed since
// Delta was called.
func Delta(t *testing.T, name string, labels map[string]string) func() float64 {
	t.Helper()
	before := Value(t, name, labels)
	return func() float64 {
		t.Helper()
		return Value(t, name, labels) - before
	}
}
