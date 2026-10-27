package evidence_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
)

var k8s *kubetest.Env

func TestMain(m *testing.M) {
	env, stop, err := kubetest.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start envtest (run `make envtest` first):", err)
		os.Exit(1)
	}
	k8s = env
	code := m.Run()
	stop()
	os.Exit(code)
}

// fakeTemplate answers with a fixed item or error.
type fakeTemplate struct {
	item evidence.Item
	err  error
}

func (fakeTemplate) Name() string { return "fake" }

func (f fakeTemplate) Collect(context.Context, client.Reader, incident.TargetRef, map[string]string) (evidence.Item, error) {
	return f.item, f.err
}

var target = incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "u-1"}

func TestRegistry_CollectFillsInMetadata(t *testing.T) {
	r := evidence.NewRegistry(fakeTemplate{item: evidence.Item{Data: []byte(`{}`), Truncated: true}})
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

	item, err := r.Collect(t.Context(), nil, "fake", target, nil, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if item.ID == "" {
		t.Error("ID is empty")
	}
	if item.Template != "fake" || item.Target != target || !item.CollectedAt.Equal(now) {
		t.Errorf("metadata = %q %v %v, want fake %v %v", item.Template, item.Target, item.CollectedAt, target, now)
	}
	if item.Classification != evidence.ClassificationInternal {
		t.Errorf("Classification = %q, want internal by default", item.Classification)
	}
	if string(item.Data) != `{}` || !item.Truncated {
		t.Error("the template's answer was not kept")
	}
}

func TestRegistry_CollectKeepsTemplateClassification(t *testing.T) {
	r := evidence.NewRegistry(fakeTemplate{item: evidence.Item{Classification: evidence.ClassificationSensitive}})
	item, err := r.Collect(t.Context(), nil, "fake", target, nil, time.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if item.Classification != evidence.ClassificationSensitive {
		t.Errorf("Classification = %q, want sensitive", item.Classification)
	}
}

func TestRegistry_CollectGivesEachItemItsOwnID(t *testing.T) {
	r := evidence.NewRegistry(fakeTemplate{})
	a, _ := r.Collect(t.Context(), nil, "fake", target, nil, time.Now())
	b, _ := r.Collect(t.Context(), nil, "fake", target, nil, time.Now())
	if a.ID == b.ID {
		t.Errorf("two items share ID %s", a.ID)
	}
}

func TestRegistry_UnknownTemplate(t *testing.T) {
	r := evidence.NewRegistry()
	if _, err := r.Collect(t.Context(), nil, "kubectl_exec", target, nil, time.Now()); !errors.Is(err, evidence.ErrUnknownTemplate) {
		t.Errorf("err = %v, want ErrUnknownTemplate", err)
	}
}

func TestRegistry_TemplateErrorIsWrapped(t *testing.T) {
	r := evidence.NewRegistry(fakeTemplate{err: evidence.ErrUnsupportedTarget})
	if _, err := r.Collect(t.Context(), nil, "fake", target, nil, time.Now()); !errors.Is(err, evidence.ErrUnsupportedTarget) {
		t.Errorf("err = %v, want ErrUnsupportedTarget", err)
	}
}

func TestBundle_ByTemplate(t *testing.T) {
	b := evidence.Bundle{Items: []evidence.Item{
		{ID: "1", Template: "pod_states"},
		{ID: "2", Template: "target_events"},
		{ID: "3", Template: "pod_states"},
	}}
	got := b.ByTemplate("pod_states")
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" {
		t.Errorf("ByTemplate = %v, want items 1 and 3", got)
	}
	if b.ByTemplate("rollout_history") != nil {
		t.Error("ByTemplate of a missing template is not empty")
	}
}
