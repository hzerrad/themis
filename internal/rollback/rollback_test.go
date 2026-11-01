package rollback_test

import (
	"errors"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/rollback"
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

func TestRead_UnknownRevision(t *testing.T) {
	target, _ := load(t)
	if _, err := rollback.Read(t.Context(), k8s, target, 7); !errors.Is(err, rollback.ErrNoSuchRevision) {
		t.Errorf("err = %v, want ErrNoSuchRevision", err)
	}
}

func TestRead_CurrentRevisionRefused(t *testing.T) {
	target, _ := load(t)
	if _, err := rollback.Read(t.Context(), k8s, target, 2); !errors.Is(err, rollback.ErrAlreadyCurrent) {
		t.Errorf("err = %v, want ErrAlreadyCurrent", err)
	}
}

func TestRead_RecreatedTarget(t *testing.T) {
	target, _ := load(t)
	target.UID = "uid-of-a-deleted-deployment"
	if _, err := rollback.Read(t.Context(), k8s, target, 1); !errors.Is(err, rollback.ErrStale) {
		t.Errorf("err = %v, want ErrStale", err)
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
		fmt.Errorf("%w: Deployment checkout is gone", rollback.ErrStale): "The workload was deleted or replaced, so the fix no longer applies.",
		fmt.Errorf("%w: 1", rollback.ErrNoSuchRevision):                  "Revision 1 no longer exists, so Themis can't roll back to it.",
		fmt.Errorf("%w: 1", rollback.ErrAlreadyCurrent):                  "The workload already runs revision 1, so there's nothing to roll back.",
	} {
		if got := rollback.Explain(err, 1); got != want {
			t.Errorf("Explain(%v) = %q, want %q", err, got, want)
		}
	}
}

// The template a rollback replaced is still a ReplicaSet's: rolling back to
// its revision puts the workload back.
func TestRevisionOf_FindsTheReplicaSetWithTheTemplate(t *testing.T) {
	target, _ := load(t)
	prior := read(t, target, 1).Desired
	if got, err := rollback.RevisionOf(t.Context(), k8s, target, prior); err != nil || got != 1 {
		t.Errorf("RevisionOf = %d, %v; want 1", got, err)
	}
	other := *prior.DeepCopy()
	other.Spec.Containers[0].Image = "example/checkout:9.9.9"
	if _, err := rollback.RevisionOf(t.Context(), k8s, target, other); !errors.Is(err, rollback.ErrNoSuchRevision) {
		t.Errorf("unknown template: err = %v, want ErrNoSuchRevision", err)
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
