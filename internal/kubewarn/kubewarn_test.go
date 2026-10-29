package kubewarn_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/kubewarn"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

const metric = "themis_kube_api_deprecation_warnings_total"

var k8s *kubetest.Env

func TestMain(m *testing.M) {
	env, stop, err := kubetest.Start(widgetCRD())
	if err != nil {
		fmt.Fprintln(os.Stderr, "start envtest (run `make envtest` first):", err)
		os.Exit(1)
	}
	k8s = env
	code := m.Run()
	stop()
	os.Exit(code)
}

// widgetCRD serves v1alpha1 as deprecated, so reading it makes the API server
// send a warning, on every Kubernetes version.
func widgetCRD() *apiextensionsv1.CustomResourceDefinition {
	schema := &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
		Type:                   "object",
		XPreserveUnknownFields: ptr.To(true),
	}}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.themis.dev"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.themis.dev",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1alpha1", Served: true, Deprecated: true, DeprecationWarning: ptr.To("example.themis.dev/v1alpha1 Widget is deprecated; use v1"), Schema: schema},
				{Name: "v1", Served: true, Storage: true, Schema: schema},
			},
		},
	}
}

func listWidgets(t *testing.T, cfg *rest.Config, version string) {
	t.Helper()
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(schema.GroupVersionKind{Group: "example.themis.dev", Version: version, Kind: "WidgetList"})
	if err := c.List(t.Context(), l, client.InNamespace("default")); err != nil {
		t.Fatal(err)
	}
}

func TestInstall_CountsAndLogsDeprecatedAPIUse(t *testing.T) {
	var buf bytes.Buffer
	cfg := rest.CopyConfig(k8s.Config)
	kubewarn.Install(cfg, slog.New(slog.NewJSONHandler(&buf, nil)))
	warned := metrictest.Delta(t, metric, nil)

	listWidgets(t, cfg, "v1alpha1")
	listWidgets(t, cfg, "v1alpha1")
	listWidgets(t, cfg, "v1")

	if got := warned(); got != 2 {
		t.Errorf("%s changed by %v, want 2: one per request to the deprecated version", metric, got)
	}
	if n := strings.Count(buf.String(), "v1alpha1 Widget is deprecated"); n != 1 {
		t.Errorf("warning logged %d times, want once:\n%s", n, buf.String())
	}
}

func TestHandler_IgnoresNonWarningCodesAndEmptyText(t *testing.T) {
	var buf bytes.Buffer
	cfg := &rest.Config{}
	kubewarn.Install(cfg, slog.New(slog.NewJSONHandler(&buf, nil)))
	warned := metrictest.Delta(t, metric, nil)

	h := cfg.WarningHandlerWithContext
	h.HandleWarningHeaderWithContext(context.Background(), 199, "agent", "miscellaneous")
	h.HandleWarningHeaderWithContext(context.Background(), 299, "agent", "")

	if got := warned(); got != 0 {
		t.Errorf("%s changed by %v, want 0", metric, got)
	}
	if buf.Len() != 0 {
		t.Errorf("logged %q, want nothing", buf.String())
	}
}
