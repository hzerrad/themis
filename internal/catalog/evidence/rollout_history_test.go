package evidence_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
)

func collectRolloutHistory(t *testing.T, target incident.TargetRef) evidence.Item {
	t.Helper()
	item, err := evidence.NewRolloutHistory().Collect(t.Context(), k8s, read(t, target), nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return item
}

// One collection of scenario 1, where revision 2 lowered the memory limit.
func TestRolloutHistory_Scenario1(t *testing.T) {
	// The client decodes times in the local zone: evidence is written in UTC.
	local := time.Local
	time.Local = time.FixedZone("UTC+1", 60*60)
	t.Cleanup(func() { time.Local = local })

	before := time.Now().Add(-time.Minute)
	item := collectRolloutHistory(t, loadScenario1(t))

	// The memory limit is what changed between the revisions: it is the clue.
	t.Run("describes every revision of the target and its template", func(t *testing.T) {
		for rs, want := range map[string]struct {
			revision string
			template []string
		}{
			"checkout-5c6a1e2f3": {"1", []string{"example/checkout:2.3.1", "256Mi"}},
			"checkout-7d9f8b6c4": {"2", []string{"example/checkout:2.4.0", "64Mi"}},
		} {
			obj := objectNamed(t, item.Data, rs)
			if got := revisionOf(t, obj); got != want.revision {
				t.Errorf("revision of %s = %s, want %s", rs, got, want.revision)
			}
			for _, s := range want.template {
				if !contains(obj, s) {
					t.Errorf("%s lacks %s: %v", rs, s, obj)
				}
			}
		}
		if strings.Contains(string(item.Data), "cart-") {
			t.Error("Data describes another workload")
		}
		if item.Truncated {
			t.Error("Truncated = true for 2 revisions")
		}
	})

	t.Run("records the rollout's progress and when the template changed", func(t *testing.T) {
		if !strings.Contains(string(item.Data), "ProgressDeadlineExceeded") {
			t.Errorf("Data lacks the Progressing condition: %s", item.Data)
		}
		if !strings.Contains(string(item.Data), `"2026-10-27T09:10:00Z"`) {
			t.Errorf("the Progressing condition's time is not in UTC: %s", item.Data)
		}
		var view evidence.RolloutHistoryView
		if err := json.Unmarshal(item.Data, &view); err != nil {
			t.Fatal(err)
		}
		if view.Deployment.TemplateChangedAt.Before(before) {
			t.Errorf("TemplateChangedAt = %v, want the time the fixture was written", view.Deployment.TemplateChangedAt)
		}
	})

	t.Run("says what the current revision changed from the one before", func(t *testing.T) {
		var view evidence.RolloutHistoryView
		if err := json.Unmarshal(item.Data, &view); err != nil {
			t.Fatal(err)
		}
		want := evidence.TemplateChangeView{From: "1", To: "2", App: []string{
			"the image of container app changed from example/checkout:2.3.1 to example/checkout:2.4.0",
			"the memory limit of container app changed from 256Mi to 64Mi",
		}, Metadata: []string{}}
		if view.Change == nil || fmt.Sprint(*view.Change) != fmt.Sprint(want) {
			t.Errorf("change = %+v, want %+v", view.Change, want)
		}
	})

	// ReplicaSet templates carry the same environment as pods.
	t.Run("leaks no environment", func(t *testing.T) {
		for _, leak := range []string{"fixture-s3cr3t-do-not-leak", "DB_PASSWORD"} {
			if strings.Contains(string(item.Data), leak) {
				t.Errorf("Data contains %s", leak)
			}
		}
	})
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

// A ReplicaSet without a revision is still listed. A rollback renumbers the
// old ReplicaSet and records its former revisions.
func TestRolloutHistory_ReplicaSetWithoutRevision(t *testing.T) {
	d := k8s.NewDeployment(t, k8s.NewNamespace(t), "checkout")
	newReplicaSetAt(t, d, "checkout-rolled-back", "5", map[string]string{"deployment.kubernetes.io/revision-history": "1,3"})
	k8s.NewReplicaSet(t, d.Namespace, "checkout-unnumbered", kubetest.RefTo(d, "apps/v1", "Deployment"))

	item := collectRolloutHistory(t, deploymentTarget(d))
	if obj := objectNamed(t, item.Data, "checkout-rolled-back"); !contains(obj, "1,3") {
		t.Errorf("the revision history is missing: %v", obj)
	}
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

// A revision shows each probe's timing, so the investigator can weigh it
// against when containers were killed, but not what the probe calls: a
// header can hold a credential.
func TestRolloutHistory_ShowsProbeTimingOnly(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d := k8s.NewDeployment(t, ns, "checkout")
	template := kubetest.PodTemplate()
	template.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/healthz", Port: intstr.FromInt32(8080),
			HTTPHeaders: []corev1.HTTPHeader{{Name: "Authorization", Value: "Bearer fixture-s3cr3t-do-not-leak"}},
		}},
		InitialDelaySeconds: 5, TimeoutSeconds: 2,
	}
	k8s.Create(t, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "checkout-probed", Labels: kubetest.PodLabels,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "1"},
			OwnerReferences: []metav1.OwnerReference{kubetest.RefTo(d, "apps/v1", "Deployment")},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: kubetest.Selector(), Template: template},
	})

	data := collectRolloutHistory(t, deploymentTarget(d)).Data
	want := `"liveness":{"failureThreshold":3,"initialDelaySeconds":5,"periodSeconds":10,"successThreshold":1,"timeoutSeconds":2}`
	if obj := objectNamed(t, data, "checkout-probed"); !contains(obj, want) {
		t.Errorf("the liveness probe's timing is missing, want %s: %v", want, obj)
	}
	for _, leak := range []string{"fixture-s3cr3t-do-not-leak", "/healthz"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("Data contains %s", leak)
		}
	}
}
