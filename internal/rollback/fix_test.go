package rollback_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/fix"
	"github.com/hzerrad/themis/internal/rollback"
)

// Going through the fix interface binds exactly what a rollback bound before
// it existed, so approvals given before an upgrade still match after it.
func TestFix_BindsWhatRollbackBound(t *testing.T) {
	live := rollback.Live{
		Target:      digest.TargetIdentity{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "u1", Generation: 3, SpecHash: "sha256:live", Manager: "kubectl"},
		DesiredHash: "sha256:desired",
	}
	plan := fix.Plan{Target: live.Target, Params: rollback.Params(1, live.DesiredHash)}
	for name, tc := range map[string]struct {
		old, now digest.ActionFields
	}{
		"a fix":   {rollback.Fields("i1", live, 1, "p1", "pd"), fix.Fields("i1", rollback.Fix{}, plan, "", "p1", "pd")},
		"an undo": {rollback.RestoreFields("i1", live, 1, "a1", "p1", "pd"), fix.Fields("i1", rollback.Fix{}, plan, "a1", "p1", "pd")},
	} {
		old, err := digest.Compute(tc.old)
		if err != nil {
			t.Fatal(err)
		}
		now, err := digest.Compute(tc.now)
		if err != nil {
			t.Fatal(err)
		}
		if old != now {
			t.Errorf("%s: digest %s through the interface, %s before it", name, now, old)
		}
	}
	if _, ok := plan.Params["restores"]; ok {
		t.Error("Fields changed the plan's own params")
	}
}

func TestRevisionParam(t *testing.T) {
	for _, tc := range []struct {
		params map[string]any
		want   bool
	}{
		{map[string]any{"revision": int64(2)}, true},
		{map[string]any{"revision": float64(2)}, true},
		{map[string]any{"revision": 2.5}, false},
		{map[string]any{"revision": int64(0)}, false},
		{map[string]any{"revision": "2"}, false},
		{map[string]any{}, false},
	} {
		if _, ok := rollback.RevisionParam(tc.params); ok != tc.want {
			t.Errorf("RevisionParam(%v) ok = %v, want %v", tc.params, ok, tc.want)
		}
	}
}
