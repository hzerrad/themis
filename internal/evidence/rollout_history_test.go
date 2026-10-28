package evidence_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
)

func collectRolloutHistory(t *testing.T, target incident.TargetRef) evidence.Item {
	t.Helper()
	item, err := evidence.NewRolloutHistory().Collect(t.Context(), k8s, target, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return item
}

func TestRolloutHistory_DescribesEveryRevisionOfTheTarget(t *testing.T) {
	item := collectRolloutHistory(t, loadScenario1(t))
	tests := map[string]string{
		"checkout-5c6a1e2f3": "1",
		"checkout-7d9f8b6c4": "2",
	}
	for rs, want := range tests {
		if got := revisionOf(t, objectNamed(t, item.Data, rs)); got != want {
			t.Errorf("revision of %s = %s, want %s", rs, got, want)
		}
	}
	if strings.Contains(string(item.Data), "cart-") {
		t.Error("Data describes another workload")
	}
	if item.Truncated {
		t.Error("Truncated = true for 2 revisions")
	}
}

// The memory limit is what changed between the revisions: it is the clue.
func TestRolloutHistory_RecordsEachRevisionsTemplate(t *testing.T) {
	item := collectRolloutHistory(t, loadScenario1(t))
	tests := map[string][]string{
		"checkout-5c6a1e2f3": {"example/checkout:2.3.1", "256Mi"},
		"checkout-7d9f8b6c4": {"example/checkout:2.4.0", "64Mi"},
	}
	for rs, wants := range tests {
		obj := objectNamed(t, item.Data, rs)
		for _, want := range wants {
			if !contains(obj, want) {
				t.Errorf("%s lacks %s: %v", rs, want, obj)
			}
		}
	}
}

func TestRolloutHistory_RecordsTheRolloutsProgress(t *testing.T) {
	item := collectRolloutHistory(t, loadScenario1(t))
	if !strings.Contains(string(item.Data), "ProgressDeadlineExceeded") {
		t.Errorf("Data lacks the Progressing condition: %s", item.Data)
	}
}

// ReplicaSet templates carry the same environment as pods.
func TestRolloutHistory_LeaksNoEnvironment(t *testing.T) {
	item := collectRolloutHistory(t, loadScenario1(t))
	for _, leak := range []string{"fixture-s3cr3t-do-not-leak", "DB_PASSWORD"} {
		if strings.Contains(string(item.Data), leak) {
			t.Errorf("Data contains %s", leak)
		}
	}
}

func TestRolloutHistory_TimesAreUTC(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC+1", 60*60)
	t.Cleanup(func() { time.Local = local })

	item := collectRolloutHistory(t, loadScenario1(t))
	if !strings.Contains(string(item.Data), `"2026-10-27T09:10:00Z"`) {
		t.Errorf("the Progressing condition's time is not in UTC: %s", item.Data)
	}
}

// newReplicaSetAt creates a ReplicaSet of d at a revision, with extra
// annotations.
func newReplicaSetAt(t *testing.T, d *appsv1.Deployment, name, revision string, annotations map[string]string) {
	t.Helper()
	a := map[string]string{"deployment.kubernetes.io/revision": revision}
	for k, v := range annotations {
		a[k] = v
	}
	k8s.Create(t, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: d.Namespace, Name: name, Labels: kubetest.PodLabels, Annotations: a,
			OwnerReferences: []metav1.OwnerReference{kubetest.RefTo(d, "apps/v1", "Deployment")},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: kubetest.Selector(), Template: kubetest.PodTemplate()},
	})
}

func deploymentTarget(d *appsv1.Deployment) incident.TargetRef {
	return incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: d.Namespace, Name: d.Name, UID: string(d.UID)}
}

// Revisions are numbers: "10" is newer than "9". The newest are kept.
func TestRolloutHistory_BoundsRevisionsKeepingTheNewest(t *testing.T) {
	d := k8s.NewDeployment(t, k8s.NewNamespace(t), "checkout")
	const revisions = 30
	for i := 1; i <= revisions; i++ {
		newReplicaSetAt(t, d, fmt.Sprintf("checkout-rev-%d", i), fmt.Sprint(i), nil)
	}

	item := collectRolloutHistory(t, deploymentTarget(d))
	if !item.Truncated {
		t.Errorf("Truncated = false for %d revisions", revisions)
	}
	data := string(item.Data)
	if !strings.Contains(data, `"checkout-rev-30"`) {
		t.Error("the newest revision was dropped")
	}
	for _, old := range []string{`"checkout-rev-9"`, `"checkout-rev-1"`} {
		if strings.Contains(data, old) {
			t.Errorf("%s was kept over newer revisions", old)
		}
	}
}

// A rollback renumbers the old ReplicaSet and records its former revisions.
func TestRolloutHistory_RecordsRevisionHistory(t *testing.T) {
	d := k8s.NewDeployment(t, k8s.NewNamespace(t), "checkout")
	newReplicaSetAt(t, d, "checkout-rolled-back", "5", map[string]string{"deployment.kubernetes.io/revision-history": "1,3"})

	item := collectRolloutHistory(t, deploymentTarget(d))
	if obj := objectNamed(t, item.Data, "checkout-rolled-back"); !contains(obj, "1,3") {
		t.Errorf("the revision history is missing: %v", obj)
	}
}

func TestRolloutHistory_ReplicaSetWithoutRevision(t *testing.T) {
	d := k8s.NewDeployment(t, k8s.NewNamespace(t), "checkout")
	newReplicaSetAt(t, d, "checkout-2", "2", nil)
	k8s.NewReplicaSet(t, d.Namespace, "checkout-unnumbered", kubetest.RefTo(d, "apps/v1", "Deployment"))

	item := collectRolloutHistory(t, deploymentTarget(d))
	objectNamed(t, item.Data, "checkout-2")
	objectNamed(t, item.Data, "checkout-unnumbered")
}

// GitOps markers are kept; the last applied configuration, which holds the
// environment, is not.
func TestRolloutHistory_RecordsOwnershipMarkers(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "checkout",
			Annotations: map[string]string{
				"argocd.argoproj.io/tracking-id":                   "shop:apps/Deployment:shop/checkout",
				"kubectl.kubernetes.io/last-applied-configuration": `{"env":"fixture-s3cr3t-do-not-leak"}`,
			},
		},
		Spec: appsv1.DeploymentSpec{Selector: kubetest.Selector(), Template: kubetest.PodTemplate()},
	}
	k8s.Create(t, d)

	data := string(collectRolloutHistory(t, deploymentTarget(d)).Data)
	if !strings.Contains(data, "argocd.argoproj.io/tracking-id") {
		t.Errorf("Data lacks the Argo CD marker: %s", data)
	}
	if strings.Contains(data, "fixture-s3cr3t-do-not-leak") {
		t.Error("Data contains the last applied configuration")
	}
}

func TestRolloutHistory_UnsupportedKind(t *testing.T) {
	target := incident.TargetRef{Group: "apps", Kind: "StatefulSet", Namespace: "shop", Name: "db", UID: "u-1"}
	if _, err := evidence.NewRolloutHistory().Collect(t.Context(), k8s, target, nil); !errors.Is(err, evidence.ErrUnsupportedTarget) {
		t.Errorf("err = %v, want ErrUnsupportedTarget", err)
	}
}

func TestRolloutHistory_DeploymentRecreated(t *testing.T) {
	target := loadScenario1(t)
	target.UID = "uid-of-the-deleted-deployment"
	if _, err := evidence.NewRolloutHistory().Collect(t.Context(), k8s, target, nil); !errors.Is(err, evidence.ErrTargetGone) {
		t.Errorf("err = %v, want ErrTargetGone", err)
	}
}
