package evidence_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hzerrad/themis/internal/evidence"
)

// A collection error says why the evidence is missing, so the operator is
// told the cause instead of "no rollout history".
func TestGap_CauseFromError(t *testing.T) {
	rs := schema.GroupResource{Group: "apps", Resource: "replicasets"}
	for name, c := range map[string]struct {
		err  error
		want evidence.GapCause
	}{
		"forbidden":            {apierrors.NewForbidden(rs, "", errors.New("RBAC")), evidence.GapForbidden},
		"forbidden, wrapped":   {fmt.Errorf("collect rollout_history: %w", apierrors.NewForbidden(rs, "", errors.New("RBAC"))), evidence.GapForbidden},
		"target gone":          {fmt.Errorf("collect: %w", evidence.ErrTargetGone), evidence.GapNotFound},
		"object not found":     {apierrors.NewNotFound(rs, "checkout-7d9f8b6c4"), evidence.GapNotFound},
		"deadline":             {fmt.Errorf("list pods: %w", context.DeadlineExceeded), evidence.GapTimeout},
		"API server timeout":   {apierrors.NewTimeoutError("slow", 1), evidence.GapTimeout},
		"anything else":        {errors.New("connection refused"), evidence.GapUnavailable},
		"API server unhealthy": {apierrors.NewServiceUnavailable("etcd"), evidence.GapUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			if got := evidence.CauseOf(c.err); got != c.want {
				t.Errorf("CauseOf = %q, want %q", got, c.want)
			}
		})
	}
}

// The causes are a closed set: each has a remedy and a metric label.
func TestGap_CausesAreClosed(t *testing.T) {
	want := []evidence.GapCause{
		evidence.GapForbidden, evidence.GapNotInProfile, evidence.GapExpired, evidence.GapNotFound,
		evidence.GapBudgetSpent, evidence.GapDataCeiling, evidence.GapTimeout, evidence.GapUnavailable,
		evidence.GapAbsent,
	}
	seen := map[evidence.GapCause]bool{}
	for _, c := range want {
		if c == "" || seen[c] {
			t.Errorf("cause %q is empty or repeated", c)
		}
		seen[c] = true
	}
	if got := evidence.GapCauses(); len(got) != len(want) {
		t.Errorf("GapCauses() has %d causes, want %d", len(got), len(want))
	}
	for _, c := range evidence.GapCauses() {
		if !seen[c] {
			t.Errorf("GapCauses() lists unknown cause %q", c)
		}
	}
}

// Only "looked, and there was nothing" is ambiguous; every other gap means
// Themis could not see.
func TestGap_OnlyAbsentIsAmbiguous(t *testing.T) {
	for _, c := range evidence.GapCauses() {
		if got, want := c.Blind(), c != evidence.GapAbsent; got != want {
			t.Errorf("%s.Blind() = %v, want %v", c, got, want)
		}
	}
}
