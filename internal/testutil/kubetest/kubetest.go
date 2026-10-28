// Package kubetest gives tests a real Kubernetes API server (envtest) and
// builders for the objects Themis reads. envtest runs no controllers, so the
// ReplicaSets and Pods a controller would create, with their owner
// references, are created by hand.
package kubetest

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Env is a client of the test API server, with object builders.
type Env struct {
	client.Client

	// Config connects to the test API server, for code that builds its own
	// clients.
	Config *rest.Config
}

// Start starts an API server with the given custom resource definitions. Call
// it from TestMain and call stop when the tests are done. Run `make envtest`
// first; KUBEBUILDER_ASSETS, if set, wins over the binaries it downloads.
func Start(crds ...*apiextensionsv1.CustomResourceDefinition) (env *Env, stop func(), err error) {
	te := &envtest.Environment{BinaryAssetsDirectory: assets(), CRDs: crds}
	cfg, err := te.Start()
	if err != nil {
		return nil, nil, err
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, nil, errors.Join(err, te.Stop())
	}
	return &Env{Client: c, Config: cfg}, func() { _ = te.Stop() }, nil
}

// assets finds the binaries `make envtest` downloaded, so tests run from an IDE
// without KUBEBUILDER_ASSETS set.
func assets() string {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	root, err := repoRoot()
	if err != nil {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(root, "bin", "envtest", "k8s", "*"))
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

// repoRoot walks up from the working directory, which is the test's package
// directory, to the directory holding go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found")
		}
		dir = parent
	}
}

// RolloutCRD is a minimal stand-in for Argo Rollouts, so tests can put a
// custom resource at the top of an owner chain.
func RolloutCRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "rollouts.argoproj.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "argoproj.io",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "rollouts", Singular: "rollout", Kind: "Rollout", ListKind: "RolloutList",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object", XPreserveUnknownFields: &preserve,
					},
				},
			}},
		},
	}
}

// PodLabels are the labels of every pod the builders create.
var PodLabels = map[string]string{"app": "checkout"}

func PodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyAlways,
		Containers:    []corev1.Container{{Name: "app", Image: "example/app:1"}},
	}
}

func PodTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: PodLabels}, Spec: PodSpec()}
}

func Selector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: PodLabels}
}

func ControllerRef(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: apiVersion, Kind: kind, Name: name, UID: uid,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
}

func RefTo(obj client.Object, apiVersion, kind string) metav1.OwnerReference {
	return ControllerRef(apiVersion, kind, obj.GetName(), obj.GetUID())
}

// NewNamespace creates a namespace with a random name, so each test works in
// its own.
func (e *Env) NewNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "t-" + uuid.NewString()[:8]}}
	e.Create(t, ns)
	return ns.Name
}

// Create creates obj, failing the test on error.
func (e *Env) Create(t *testing.T, obj client.Object) {
	t.Helper()
	if err := e.Client.Create(t.Context(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func (e *Env) NewDeployment(t *testing.T, ns, name string) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       appsv1.DeploymentSpec{Selector: Selector(), Template: PodTemplate()},
	}
	e.Create(t, d)
	return d
}

func (e *Env) NewReplicaSet(t *testing.T, ns, name string, owners ...metav1.OwnerReference) *appsv1.ReplicaSet {
	t.Helper()
	rs := &appsv1.ReplicaSet{
		// The Deployment controller copies the pod template's labels onto
		// the ReplicaSet, and finds its ReplicaSets with its selector.
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: PodLabels, OwnerReferences: owners},
		Spec:       appsv1.ReplicaSetSpec{Selector: Selector(), Template: PodTemplate()},
	}
	e.Create(t, rs)
	return rs
}

func (e *Env) NewPod(t *testing.T, ns, name string, owners ...metav1.OwnerReference) *corev1.Pod {
	t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: PodLabels, OwnerReferences: owners},
		Spec:       PodSpec(),
	}
	e.Create(t, p)
	return p
}

// Load creates the objects of a multi-document YAML file, relative to the
// repository root, in namespace ns, in file order, and returns them.
//
// Owner references and Event involvedObjects name their object by kind and
// name; Load fills in the UID of the object created earlier in the same file. Each object's status is
// written after it is created, since the API server ignores status on create
// and envtest has no controllers or kubelet to set it.
func (e *Env) Load(t *testing.T, ns, path string) []*unstructured.Unstructured {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("find repository root: %v", err)
	}
	f, err := os.Open(filepath.Join(root, path)) //nolint:gosec // a fixture path chosen by the test
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	uids := map[string]types.UID{} // kind/name → UID
	var objs []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		u.SetNamespace(ns)
		refs := u.GetOwnerReferences()
		for i, ref := range refs {
			uid, ok := uids[ref.Kind+"/"+ref.Name]
			if !ok {
				t.Fatalf("%s %s: owner %s %s must come earlier in %s", u.GetKind(), u.GetName(), ref.Kind, ref.Name, path)
			}
			refs[i].UID = uid
		}
		u.SetOwnerReferences(refs)
		if u.GetKind() == "Event" {
			linkInvolvedObject(t, u, ns, uids)
		}

		status, hasStatus := u.Object["status"]
		e.Create(t, u)
		if hasStatus {
			u.Object["status"] = status
			if err := e.Status().Update(t.Context(), u); err != nil {
				t.Fatalf("update status of %s %s: %v", u.GetKind(), u.GetName(), err)
			}
		}
		uids[u.GetKind()+"/"+u.GetName()] = u.GetUID()
		objs = append(objs, u)
	}
	return objs
}

// linkInvolvedObject points an Event at the object of the same kind and name
// created earlier, in namespace ns.
func linkInvolvedObject(t *testing.T, u *unstructured.Unstructured, ns string, uids map[string]types.UID) {
	t.Helper()
	kind, _, _ := unstructured.NestedString(u.Object, "involvedObject", "kind")
	name, _, _ := unstructured.NestedString(u.Object, "involvedObject", "name")
	uid, ok := uids[kind+"/"+name]
	if !ok {
		t.Fatalf("Event %s: involved %s %s must come earlier", u.GetName(), kind, name)
	}
	_ = unstructured.SetNestedField(u.Object, ns, "involvedObject", "namespace")
	_ = unstructured.SetNestedField(u.Object, string(uid), "involvedObject", "uid")
}

// NewRollout creates Deployment → ReplicaSet → Pod, as the Deployment
// controller would.
func (e *Env) NewRollout(t *testing.T, ns, name string) (*appsv1.Deployment, *corev1.Pod) {
	t.Helper()
	d := e.NewDeployment(t, ns, name)
	rs := e.NewReplicaSet(t, ns, name+"-7d9f", RefTo(d, "apps/v1", "Deployment"))
	p := e.NewPod(t, ns, name+"-7d9f-x2k", RefTo(rs, "apps/v1", "ReplicaSet"))
	return d, p
}
