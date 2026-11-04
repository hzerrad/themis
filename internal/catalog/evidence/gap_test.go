package evidence_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/workload"
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
		"target replaced":      {&workload.Error{Case: workload.ErrReplaced}, evidence.GapNotFound},
		"pod replaced":         {fmt.Errorf("collect: %w", evidence.ErrPodReplaced), evidence.GapNotFound},
		"no pods":              {evidence.ErrNoPods, evidence.GapAbsent},
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

// Only "looked, and there was nothing" is ambiguous; every other gap means
// Themis could not see.
func TestGap_OnlyAbsentIsAmbiguous(t *testing.T) {
	for _, c := range evidence.GapCauses() {
		if got, want := c.Blind(), c != evidence.GapAbsent; got != want {
			t.Errorf("%s.Blind() = %v, want %v", c, got, want)
		}
	}
}
