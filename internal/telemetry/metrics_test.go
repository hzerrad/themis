package telemetry_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/telemetry"
)

// A value outside the set is recorded as other, so a label never grows past
// what Themis knows.
func TestMetrics_ConfigLabelsBounded(t *testing.T) {
	b := telemetry.Bounded{"deployment.rollback-to-revision"}
	if got := b.Value("deployment.rollback-to-revision"); got != "deployment.rollback-to-revision" {
		t.Errorf("Value of a known capability = %q", got)
	}
	for _, v := range []string{"", "deployment.scale", "kube-system/coredns"} {
		if got := b.Value(v); got != telemetry.Other {
			t.Errorf("Value(%q) = %q, want %q", v, got, telemetry.Other)
		}
	}
}
