package rollback_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

func load(t *testing.T) (incident.TargetRef, *appsv1.Deployment) {
	t.Helper()
	ns := k8s.NewNamespace(t)
	k8s.Load(t, ns, scenario1)
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	return incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)}, &d
}

func read(t *testing.T, target incident.TargetRef, revision int64) rollback.Live {
	t.Helper()
	live, err := rollback.Read(t.Context(), k8s, target, revision)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return live
}

func digestOf(t *testing.T, live rollback.Live, revision int64) string {
	t.Helper()
	d, err := digest.Compute(rollback.Fields("inc-1", live, revision, "approve", "sha256:p"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRead_DesiredIsThePriorTemplateWithoutItsHash(t *testing.T) {
	target, _ := load(t)
	live := read(t, target, 1)

	if got := live.Desired.Spec.Containers[0].Image; got != "example/checkout:2.3.1" {
		t.Errorf("desired image = %s, want revision 1's", got)
	}
	if _, ok := live.Desired.Labels["pod-template-hash"]; ok {
		t.Error("the desired template keeps pod-template-hash")
	}
	if got := live.PreState.Spec.Containers[0].Image; got != "example/checkout:2.4.0" {
		t.Errorf("pre-state image = %s, want the current template", got)
	}
	if live.Target.UID != target.UID || live.Target.SpecHash == "" || live.DesiredHash == "" {
		t.Errorf("identity = %+v", live.Target)
	}
	if live.Replicas != 3 {
		t.Errorf("Replicas = %d, want 3", live.Replicas)
	}
}

// Read refuses a rollback that has nothing to roll back to.
func TestRead_Refused(t *testing.T) {
	loaded, _ := load(t)
	tests := []struct {
		name     string
		uid      string
		revision int64
		want     error
	}{
		{"unknown revision", "", 7, rollback.ErrNoSuchRevision},
		{"current revision", "", 2, rollback.ErrAlreadyCurrent},
		{"recreated target", "uid-of-a-deleted-deployment", 1, rollback.ErrStale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := loaded
			if tt.uid != "" {
				target.UID = tt.uid
			}
			if _, err := rollback.Read(t.Context(), k8s, target, tt.revision); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDigest_StableAcrossStatusChanges(t *testing.T) {
	target, d := load(t)
	before := digestOf(t, read(t, target, 1), 1)

	d.Status.ReadyReplicas, d.Status.AvailableReplicas = 1, 1
	if err := k8s.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if after := digestOf(t, read(t, target, 1), 1); after != before {
		t.Error("a status update changed the digest")
	}
}

func TestDigest_ChangesWithTheTemplate(t *testing.T) {
	target, d := load(t)
	before := digestOf(t, read(t, target, 1), 1)

	d.Spec.Template.Spec.Containers[0].Resources.Limits["memory"] = corev1.MustParse("128Mi")
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if after := digestOf(t, read(t, target, 1), 1); after == before {
		t.Error("a template change left the digest unchanged")
	}
}

func TestApply_RestoresPriorTemplate(t *testing.T) {
	target, _ := load(t)
	live := read(t, target, 1)

	gen, err := rollback.Apply(t.Context(), k8s.Client, live, false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, &d); err != nil {
		t.Fatal(err)
	}
	if got := d.Spec.Template.Spec.Containers[0].Image; got != "example/checkout:2.3.1" {
		t.Errorf("image after rollback = %s", got)
	}
	if mem := d.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String(); mem != "256Mi" {
		t.Errorf("memory limit after rollback = %s", mem)
	}
	if gen != d.Generation {
		t.Errorf("Apply returned generation %d, want %d", gen, d.Generation)
	}
}

func TestApply_DryRunWritesNothing(t *testing.T) {
	target, before := load(t)
	if _, err := rollback.Apply(t.Context(), k8s.Client, read(t, target, 1), true); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, &d); err != nil {
		t.Fatal(err)
	}
	if d.Generation != before.Generation {
		t.Error("a dry run changed the Deployment")
	}
}

// A write since the read fails the optimistic lock instead of being lost.
func TestApply_ConcurrentChangeConflicts(t *testing.T) {
	target, d := load(t)
	live := read(t, target, 1)
	d.Spec.Template.Annotations = map[string]string{"edited": "by-a-human"}
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := rollback.Apply(t.Context(), k8s.Client, live, false); err == nil {
		t.Error("Apply overwrote a concurrent change")
	}
}

// A refused rollback is explained to the operator in words, never as an error.
func TestExplain(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("%w: %w", rollback.ErrStale, &workload.Error{Case: workload.ErrGone}):        "The workload was deleted, so the fix no longer applies.",
		fmt.Errorf("%w: %w", rollback.ErrStale, &workload.Error{Case: workload.ErrReplaced}):    "The workload was replaced by a new one with the same name, so the fix no longer applies.",
		fmt.Errorf("%w: %w", rollback.ErrStale, &workload.Error{Case: workload.ErrUnsupported}): "Themis can only roll back Deployments.",
		fmt.Errorf("%w: 1", rollback.ErrNoSuchRevision):                                         "Revision 1 no longer exists, so Themis can't roll back to it.",
		fmt.Errorf("%w: 1", rollback.ErrAlreadyCurrent):                                         "Nothing to roll back. The workload already runs revision 1.",
	} {
		if got := rollback.Explain(err, 1); got != want {
			t.Errorf("Explain(%v) = %q, want %q", err, got, want)
		}
	}
}

// The template a rollback replaced is still a ReplicaSet's, found by its
// hash: rolling back to its revision puts the workload back.
func TestRevisionWith_FindsTheReplicaSetWithTheTemplate(t *testing.T) {
	target, _ := load(t)
	live := read(t, target, 1)
	if got, err := rollback.RevisionWith(t.Context(), k8s, target, live.DesiredHash); err != nil || got != 1 {
		t.Errorf("RevisionWith = %d, %v; want 1", got, err)
	}
	if _, err := rollback.RevisionWith(t.Context(), k8s, target, "sha256:no-such-template"); !errors.Is(err, rollback.ErrNoSuchRevision) {
		t.Errorf("unknown template: err = %v, want ErrNoSuchRevision", err)
	}
}

// A rollback or partial rollback an earlier version of Themis stored with
// whole pod templates keeps, once upgraded, what people saw of it, no
// secret of either template, and what putting it back needs: the revision
// it replaced, found by its template's hash.
func TestRecord_OfStoredTemplates(t *testing.T) {
	target, _ := load(t)
	live := read(t, target, 1)
	pre, _ := json.Marshal(live.PreState)
	earlier, _ := json.Marshal(live.Desired)
	partial := *live.PreState.DeepCopy()
	partial.Spec.Containers[0].Resources.Limits["memory"] = corev1.MustParse("256Mi")
	after, _ := json.Marshal(partial)
	limit := "the memory limit of container app changed from 256Mi to 64Mi"

	for _, tt := range []struct {
		fix           capability.Capability
		params        map[string]any
		before, after json.RawMessage
		act, note     string
	}{
		{
			rollback.Capability{},
			map[string]any{"revision": int64(1)},
			pre, earlier,
			"Roll checkout back to revision 1", "Rolling back undoes 2 changes, including the image.",
		},
		{
			rollback.Partial{},
			map[string]any{"revision": int64(1), "changes": []any{limit}},
			json.RawMessage(`{"template":` + string(pre) + `,"earlier":` + string(earlier) + `}`), after,
			"Roll back checkout's memory limit to revision 1", "Keeps the rollout's new image.",
		},
	} {
		record, err := tt.fix.(capability.Templating).Record(tt.params, tt.before, tt.after)
		if err != nil {
			t.Fatalf("%s: %v", tt.fix.Name(), err)
		}
		if strings.Contains(string(record), "fixture-s3cr3t-do-not-leak") {
			t.Errorf("%s: the record keeps the literal DB_PASSWORD: %s", tt.fix.Name(), record)
		}
		changes, err := tt.fix.Changes(record)
		if err != nil || !strings.Contains(fmt.Sprint(changes), "limits") {
			t.Errorf("%s: changes %+v, %v; want the memory limit", tt.fix.Name(), changes, err)
		}
		if w := tt.fix.Words("checkout", tt.params, record); w.Act != tt.act || w.Note != tt.note {
			t.Errorf("%s: words %+v, want %q noting %q", tt.fix.Name(), w, tt.act, tt.note)
		}
		if name, params, err := tt.fix.Undo(t.Context(), k8s, target, record); err != nil || name != rollback.Name || params["revision"] != int64(2) {
			t.Errorf("%s: Undo = %s %v, %v; want a rollback to revision 2", tt.fix.Name(), name, params, err)
		}
	}
	if _, err := (rollback.Capability{}).Record(nil, json.RawMessage(`[]`), earlier); err == nil {
		t.Error("Record read a template that isn't one")
	}
}

// What a restore undoes is part of its digest: a restore and a rollback to
// the same revision are different actions.
func TestRestoreFields_BindWhatIsUndone(t *testing.T) {
	target, _ := load(t)
	live := read(t, target, 1)
	restore, err := digest.Compute(rollback.RestoreFields("inc-1", live, 1, "act-1", "approve", "sha256:p"))
	if err != nil {
		t.Fatal(err)
	}
	if restore == digestOf(t, live, 1) {
		t.Error("a restore has the digest of a plain rollback")
	}
}
