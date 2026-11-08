package rollback_test

import (
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/digest"
)

// Going through the fix interface binds exactly what a rollback bound before
// it existed, so approvals given before an upgrade still match after it.
func TestFix_BindsWhatRollbackBound(t *testing.T) {
	live := rollback.Live{
		Target:      digest.TargetIdentity{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "u1", Generation: 3, SpecHash: "sha256:live", Manager: "kubectl"},
		DesiredHash: "sha256:desired",
	}
	plan := capability.Plan{Target: live.Target, Params: rollback.Params(1, live.DesiredHash)}
	for name, tc := range map[string]struct {
		old, now digest.ActionFields
	}{
		"a fix":   {rollback.Fields("i1", live, 1, "p1", "pd"), capability.Fields("i1", rollback.Capability{}, plan, "", "p1", "pd")},
		"an undo": {rollback.RestoreFields("i1", live, 1, "a1", "p1", "pd"), capability.Fields("i1", rollback.Capability{}, plan, "a1", "p1", "pd")},
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

// A rollback is named by the revision it goes back to.
func TestWords(t *testing.T) {
	w := rollback.Capability{}.Words("checkout", map[string]any{"revision": int64(1)}, nil)
	if w.Noun != "rollback" || w.Act != "Roll checkout back to revision 1" || w.Done != "Rolled checkout back to revision 1." {
		t.Errorf("Words = %+v", w)
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

// A rollback that undoes several changes says how many, and that the image
// is one, and shows the digest each version's pods run: nobody takes back
// new code without seeing it.
func TestRollback_SaysWhatItUndoes(t *testing.T) {
	target := loadPartial(t)
	p, err := rollback.Capability{}.Plan(t.Context(), k8s, target, map[string]any{"revision": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if w := (rollback.Capability{}).Words("checkout", p.Params, p.Record); w.Note != "Rolling back undoes 6 changes, including the image." {
		t.Errorf("Note = %q", w.Note)
	}
	more, err := rollback.Capability{}.Describe(t.Context(), k8s, p)
	if err != nil {
		t.Fatal(err)
	}
	images, _ := more["images"].(map[string]any)
	if app, _ := images["app"].(map[string]any); app["before"] != "sha256:"+strings.Repeat("2", 64) || app["after"] != "sha256:"+strings.Repeat("1", 64) {
		t.Errorf("Describe = %v, want each revision's digest", more)
	}
}
