package intake_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// k8s talks to a real API server started once for the package. Each test
// works in its own namespace.
var k8s client.Client

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	env := &envtest.Environment{
		BinaryAssetsDirectory: envtestAssets(),
		CRDs:                  []*apiextensionsv1.CustomResourceDefinition{rolloutCRD()},
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start envtest (run `make envtest` first):", err)
		return 1
	}
	defer func() { _ = env.Stop() }()

	k8s, err = client.New(cfg, client.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "create client:", err)
		return 1
	}
	return m.Run()
}

// envtestAssets lets tests run from an IDE without KUBEBUILDER_ASSETS set, by
// finding the binaries `make envtest` downloaded. The env var still wins.
func envtestAssets() string {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "envtest", "k8s", "*"))
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

// rolloutCRD is a minimal stand-in for Argo Rollouts, so tests can put a
// custom resource at the top of an owner chain.
func rolloutCRD() *apiextensionsv1.CustomResourceDefinition {
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

func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "t-" + uuid.NewString()[:8]}}
	if err := k8s.Create(t.Context(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}
