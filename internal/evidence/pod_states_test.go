package evidence_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

// These tests do not depend on the shape of pod_states' JSON. They look for
// the JSON object describing a pod (the one holding the pod's name) and check
// what is inside it.

// loadScenario1 loads the scenario 1 cluster and returns the checkout
// Deployment as a target.
func loadScenario1(t *testing.T) incident.TargetRef {
	t.Helper()
	ns := k8s.NewNamespace(t)
	for _, obj := range k8s.Load(t, ns, scenario1) {
		if obj.GetKind() == "Deployment" && obj.GetName() == "checkout" {
			return targetOf(obj)
		}
	}
	t.Fatal("scenario 1 has no checkout Deployment")
	return incident.TargetRef{}
}

func targetOf(obj *unstructured.Unstructured) incident.TargetRef {
	gvk := obj.GroupVersionKind()
	return incident.TargetRef{
		Group: gvk.Group, Kind: gvk.Kind,
		Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID()),
	}
}

func collectPodStates(t *testing.T, target incident.TargetRef) evidence.Item {
	t.Helper()
	item, err := evidence.NewPodStates().Collect(t.Context(), k8s, target, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !json.Valid(item.Data) {
		t.Fatalf("Data is not valid JSON: %s", item.Data)
	}
	return item
}

// podObject returns the innermost JSON object that has a string field equal to
// the pod's name.
func podObject(t *testing.T, data []byte, pod string) any {
	t.Helper()
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("decode Data: %v", err)
	}
	var found any
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for _, field := range v {
				if s, ok := field.(string); ok && s == pod {
					found = v
				}
			}
			for _, field := range v {
				walk(field)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(root)
	if found == nil {
		t.Fatalf("no JSON object for pod %s in %s", pod, data)
	}
	return found
}

var revisionKey = regexp.MustCompile(`(?i)revision`)

// revisionOf finds a field whose name contains "revision" in the pod's object.
func revisionOf(t *testing.T, obj any) string {
	t.Helper()
	for k, v := range obj.(map[string]any) {
		if revisionKey.MatchString(k) {
			return fmt.Sprint(v)
		}
	}
	t.Fatalf("pod object has no revision field: %v", obj)
	return ""
}

func contains(obj any, s string) bool {
	b, _ := json.Marshal(obj)
	return strings.Contains(string(b), s)
}

func TestPodStates_DescribesEveryPodOfTheTarget(t *testing.T) {
	item := collectPodStates(t, loadScenario1(t))
	for _, pod := range []string{
		"checkout-5c6a1e2f3-a1b2c", "checkout-5c6a1e2f3-d3e4f", "checkout-5c6a1e2f3-g5h6i",
		"checkout-7d9f8b6c4-x2kqp",
	} {
		podObject(t, item.Data, pod)
	}
	if strings.Contains(string(item.Data), "cart-6b7c8d9e0-z9y8x") {
		t.Error("Data describes a pod of another workload")
	}
	if item.Truncated {
		t.Error("Truncated = true for 4 pods")
	}
}

func TestPodStates_RecordsEachPodsRevision(t *testing.T) {
	item := collectPodStates(t, loadScenario1(t))
	tests := map[string]string{
		"checkout-5c6a1e2f3-a1b2c": "1",
		"checkout-7d9f8b6c4-x2kqp": "2",
	}
	for pod, want := range tests {
		if got := revisionOf(t, podObject(t, item.Data, pod)); got != want {
			t.Errorf("revision of %s = %s, want %s", pod, got, want)
		}
	}
}

// The OOM kill is in the container's last state: its current state only says
// it is backing off.
func TestPodStates_RecordsLastTermination(t *testing.T) {
	item := collectPodStates(t, loadScenario1(t))
	crashing := podObject(t, item.Data, "checkout-7d9f8b6c4-x2kqp")
	for _, want := range []string{"OOMKilled", "CrashLoopBackOff"} {
		if !contains(crashing, want) {
			t.Errorf("the crashing pod's object lacks %s: %v", want, crashing)
		}
	}
	if healthy := podObject(t, item.Data, "checkout-5c6a1e2f3-a1b2c"); contains(healthy, "OOMKilled") {
		t.Errorf("a healthy pod is reported OOMKilled: %v", healthy)
	}
}

// Pod specs carry environment variables, often literal secrets. pod_states
// describes state, never specs.
func TestPodStates_LeaksNoEnvironment(t *testing.T) {
	item := collectPodStates(t, loadScenario1(t))
	for _, leak := range []string{"fixture-s3cr3t-do-not-leak", "DB_PASSWORD"} {
		if strings.Contains(string(item.Data), leak) {
			t.Errorf("Data contains %s", leak)
		}
	}
}

// The client decodes times in the local time zone. Evidence must not depend on
// where Themis runs.
func TestPodStates_TimesAreUTC(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC+1", 60*60)
	t.Cleanup(func() { time.Local = local })

	item := collectPodStates(t, loadScenario1(t))
	crashing := podObject(t, item.Data, "checkout-7d9f8b6c4-x2kqp")
	if !contains(crashing, `"2026-10-27T09:14:02Z"`) {
		t.Errorf("the OOM kill's finish time is not in UTC: %v", crashing)
	}
}

func TestPodStates_BoundsThePodCount(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d := k8s.NewDeployment(t, ns, "checkout")
	rs := k8s.NewReplicaSet(t, ns, "checkout-7d9f", kubetest.RefTo(d, "apps/v1", "Deployment"))
	const pods = 53
	for i := range pods {
		k8s.NewPod(t, ns, fmt.Sprintf("checkout-7d9f-%03d", i), kubetest.RefTo(rs, "apps/v1", "ReplicaSet"))
	}
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: d.Name, UID: string(d.UID)}

	item := collectPodStates(t, target)
	if !item.Truncated {
		t.Errorf("Truncated = false for %d pods", pods)
	}
	n := 0
	for i := range pods {
		if strings.Contains(string(item.Data), fmt.Sprintf(`"checkout-7d9f-%03d"`, i)) {
			n++
		}
	}
	if n == 0 || n >= pods {
		t.Errorf("Data describes %d pods, want some but fewer than %d", n, pods)
	}
}

func TestPodStates_UnsupportedKind(t *testing.T) {
	target := incident.TargetRef{Group: "apps", Kind: "StatefulSet", Namespace: "shop", Name: "db", UID: "u-1"}
	_, err := evidence.NewPodStates().Collect(t.Context(), k8s, target, nil)
	if !errors.Is(err, evidence.ErrUnsupportedTarget) {
		t.Errorf("err = %v, want ErrUnsupportedTarget", err)
	}
}

func TestPodStates_DeploymentGone(t *testing.T) {
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: k8s.NewNamespace(t), Name: "checkout", UID: "u-1"}
	if _, err := evidence.NewPodStates().Collect(t.Context(), k8s, target, nil); err == nil {
		t.Error("Collect succeeded for a missing Deployment")
	}
}

// A Deployment deleted and recreated under the same name is another object:
// its pods are not evidence about the incident's target.
func TestPodStates_DeploymentRecreated(t *testing.T) {
	target := loadScenario1(t)
	target.UID = "uid-of-the-deleted-deployment"
	if _, err := evidence.NewPodStates().Collect(t.Context(), k8s, target, nil); err == nil {
		t.Error("Collect succeeded for a recreated Deployment")
	}
}
