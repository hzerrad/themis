package rollback_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/incident"
)

const partialFixture = "internal/catalog/capability/rollback/testdata/partial.yaml"

// The rollout's changes, as rollout_history's change.app says them.
const (
	modeChanged  = "the environment variable MODE of container app changed"
	dbURLChanged = "the environment variable DB_URL of container app changed"
	imageChanged = "the image of container app changed from example/checkout:2.3.1 to example/checkout:2.4.0"
	cacheAdded   = "the volume cache was added"
	mountsChange = "the volume mounts of container app changed"
)

func loadPartial(t *testing.T) incident.TargetRef {
	t.Helper()
	ns := k8s.NewNamespace(t)
	k8s.Load(t, ns, partialFixture)
	d := k8s.Deployment(t, ns, "checkout")
	return incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)}
}

func planPartial(t *testing.T, target incident.TargetRef, revision int64, changes ...any) capability.Plan {
	t.Helper()
	p, err := rollback.Partial{}.Plan(t.Context(), k8s, target, map[string]any{"revision": revision, "changes": changes})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return p
}

func container(t *testing.T, raw json.RawMessage) corev1.Container {
	t.Helper()
	var tpl corev1.PodTemplateSpec
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	return tpl.Spec.Containers[0]
}

func env(c corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

// A partial rollback puts back only the changes named, as the revision before
// had them, a Secret reference as the reference, and keeps every other change
// of the rollout, the new image first. What it changes is shown with values
// that look secret hidden, and once applied, putting it back is a rollback to
// the revision it replaced.
func TestPartial_RollsBackOnlyWhatIsNamed(t *testing.T) {
	target := loadPartial(t)
	// Named as people copy them: capitalised, with a full stop, or by key.
	p := planPartial(t, target, 1, "The environment variable MODE of container app changed.", "container.app.env.DB_URL")

	got := container(t, p.After)
	if got.Image != "example/checkout:2.4.0" || env(got, "LOG_LEVEL") == nil || len(got.VolumeMounts) != 1 {
		t.Errorf("the rollout's other changes weren't kept: %+v", got)
	}
	if e := env(got, "MODE"); e == nil || e.Value != "healthy" {
		t.Errorf("MODE = %+v, want revision 1's healthy", e)
	}
	if e := env(got, "DB_URL"); e == nil || e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "db" {
		t.Errorf("DB_URL = %+v, want revision 1's Secret reference", e)
	}
	if fmt.Sprint(p.Params["changes"]) != fmt.Sprint([]any{modeChanged, dbURLChanged}) || p.Params["templateHash"] != p.Mark {
		t.Errorf("Params = %v, want the changes as the rollout history says them, and the template bound", p.Params)
	}
	if p.Facts["image"] != "kept" || p.Facts["all"] != "false" || p.Facts["container.app.env.MODE"] != "rolled_back" {
		t.Errorf("Facts = %v", p.Facts)
	}
	if p.BlastRadius["deployedBy"] != "Helm" {
		t.Errorf("BlastRadius = %v, want deployedBy Helm", p.BlastRadius)
	}

	changes, err := rollback.Partial{}.Changes(p.Before, p.After)
	if err != nil {
		t.Fatal(err)
	}
	shown := fmt.Sprint(changes)
	if len(changes) != 2 || !strings.Contains(shown, "strict healthy") || !strings.Contains(shown, "from secret db/url") || strings.Contains(shown, "s3cr3t") {
		t.Errorf("Changes = %+v, want MODE and DB_URL with the literal hidden", changes)
	}

	w := rollback.Partial{}.Words("checkout", p.Params, p.Before, p.After)
	if w.Noun != "partial rollback" || w.Act != "Roll back checkout's environment variable MODE and environment variable DB_URL to revision 1" ||
		w.Note != "Keeps the rollout's other 4 changes, including the new image." {
		t.Errorf("Words = %+v", w)
	}
	if p.Done != "Rolled back checkout's environment variable MODE and environment variable DB_URL to revision 1." {
		t.Errorf("Done = %q", p.Done)
	}
	if more, err := (rollback.Partial{}).Describe(t.Context(), k8s, p); err != nil || more != nil {
		t.Errorf("Describe = %v, %v; want nothing, the image isn't rolled back", more, err)
	}

	if _, err := (rollback.Partial{}).Apply(t.Context(), k8s.Client, p, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, &d); err != nil {
		t.Fatal(err)
	}
	if mark, _ := (rollback.Partial{}).Mark(t.Context(), k8s, target); mark != p.Mark {
		t.Errorf("the live template's mark is %s, want the plan's %s", mark, p.Mark)
	}
	name, params, err := rollback.Partial{}.Undo(t.Context(), k8s, target, p.Before)
	if err != nil || name != rollback.Name || params["revision"] != int64(2) {
		t.Errorf("Undo = %s %v, %v; want a rollback to revision 2", name, params, err)
	}
}

// Rolling back the image too shows the digest each version's pods run, and
// rolling back every change says it is the same as a rollback.
func TestPartial_ImageAndEveryChange(t *testing.T) {
	target := loadPartial(t)
	p := planPartial(t, target, 1, imageChanged)
	more, err := rollback.Partial{}.Describe(t.Context(), k8s, p)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(more) != "map[images:map[app:map[after:sha256:"+strings.Repeat("1", 64)+" before:sha256:"+strings.Repeat("2", 64)+"]]]" {
		t.Errorf("Describe = %v, want the digests of revision 2's image before and revision 1's after", more)
	}
	if note := (rollback.Partial{}).Words("checkout", p.Params, p.Before, p.After).Note; p.Facts["image"] != "rolled_back" || note != "Keeps the rollout's other 5 changes." {
		t.Errorf("Facts = %v", p.Facts)
	}

	// Keeping one change names it: an approver sees what stays as it is.
	logLevel := "the environment variable LOG_LEVEL of container app was added"
	allButMode := planPartial(t, target, 1, imageChanged, dbURLChanged, logLevel, mountsChange, cacheAdded)
	if note := (rollback.Partial{}).Words("checkout", allButMode.Params, allButMode.Before, allButMode.After).Note; note != "Keeps the rollout's change to the environment variable MODE of container app." {
		t.Errorf("Note = %q", note)
	}

	all := planPartial(t, target, 1, imageChanged, modeChanged, dbURLChanged, logLevel, mountsChange, cacheAdded)
	w := (rollback.Partial{}).Words("checkout", all.Params, all.Before, all.After)
	if all.Facts["all"] != "true" || w.Act != "Roll back 6 of checkout's changes to revision 1" ||
		w.Note != "Rolls back every change of the rollout to what the pods run, the same as a rollback to revision 1." {
		t.Errorf("every change: Facts = %v, Words = %+v", all.Facts, w)
	}
}

// Themis refuses a partial rollback naming anything but a change the latest
// rollout made to what the pods run, going back to anything but the revision
// before it, or leaving a mount without its volume.
func TestPartial_Refused(t *testing.T) {
	target := loadPartial(t)
	for _, tc := range []struct {
		name     string
		revision int64
		changes  []any
		want     string
		noOp     bool
	}{
		{
			"a change the rollout didn't make", 1,
			[]any{modeChanged, "the memory limit of container app changed from 256Mi to 128Mi"},
			`"the memory limit of container app changed from 256Mi to 128Mi" isn't one of the changes revision 2 made to what the pods run. They are: "` + imageChanged + `", "` + modeChanged + `"`, false,
		},
		{"the current revision", 2, []any{modeChanged}, "Nothing to roll back. The workload already runs revision 2.", true},
		{"a revision Kubernetes no longer keeps", 7, []any{modeChanged}, "Revision 7 no longer exists, so Themis can't roll back to it.", false},
		{
			"a volume still mounted", 1,
			[]any{cacheAdded},
			"Rolling back only these changes leaves container app mounting the volume cache, which the pod would no longer have. Roll back the volume cache and the volume mounts of container app together.", false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rollback.Partial{}.Plan(t.Context(), k8s, target, map[string]any{"revision": tc.revision, "changes": tc.changes})
			var refusal *capability.Refusal
			if !errors.As(err, &refusal) || !strings.HasPrefix(refusal.Reason, tc.want) || refusal.NoOp != tc.noOp {
				t.Errorf("err = %v, want a refusal starting %q", err, tc.want)
			}
		})
	}
	gone := target
	gone.UID = "another"
	if _, err := (rollback.Partial{}).Plan(t.Context(), k8s, gone, map[string]any{"revision": int64(1), "changes": []any{modeChanged}}); err == nil ||
		err.Error() != "The workload was replaced by a new one with the same name, so the fix no longer applies." {
		t.Errorf("a replaced workload: err = %v", err)
	}
}

// A suggestion names a revision and the changes, nothing else, and each
// change once.
func TestPartial_Validate(t *testing.T) {
	got, err := rollback.Partial{}.Validate(map[string]any{"revision": float64(1), "changes": []any{" " + modeChanged, modeChanged}})
	if err != nil || fmt.Sprint(got) != fmt.Sprint(map[string]any{"revision": int64(1), "changes": []any{modeChanged}}) {
		t.Errorf("Validate = %v, %v", got, err)
	}
	for _, params := range []map[string]any{
		{"revision": float64(1)},
		{"revision": float64(1), "changes": []any{}},
		{"revision": float64(1), "changes": []any{""}},
		{"revision": float64(1), "changes": []any{3}},
		{"revision": float64(1), "changes": modeChanged},
		{"changes": []any{modeChanged}},
		{"revision": float64(1), "changes": []any{modeChanged}, "values": map[string]any{"MODE": "healthy"}},
	} {
		if _, err := (rollback.Partial{}).Validate(params); !errors.Is(err, capability.ErrInvalidParams) {
			t.Errorf("Validate(%v) err = %v, want invalid parameters", params, err)
		}
	}
}

// Without the cluster, the words count the changes a suggestion names.
func TestPartial_WordsOfASuggestion(t *testing.T) {
	w := rollback.Partial{}.Words("checkout", map[string]any{"revision": int64(1), "changes": []any{modeChanged}}, nil, nil)
	if w.Act != "Roll back one of checkout's changes to revision 1" || w.Note != "" {
		t.Errorf("Words = %+v", w)
	}
}
