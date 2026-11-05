package resources_test

import (
	"reflect"
	"testing"

	"github.com/hzerrad/themis/internal/catalog/capability/resources"
	"github.com/hzerrad/themis/internal/incident"
)

// Against a real API server: a dry run writes nothing, the fix writes the
// values it planned and leaves the mark it recorded, and undoing it puts the
// values back as they were.
func TestApplyAndUndo(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d := checkout()
	d.Namespace, d.UID, d.Generation = ns, "", 0
	k8s.Create(t, d)
	tt := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)}
	ctx := t.Context()

	p, err := fix.Plan(ctx, k8s, tt, set("app", map[string]string{"limits.memory": "0.5Gi"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fix.Apply(ctx, k8s.Client, p, true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := k8s.Deployment(t, ns, "checkout"); got.Generation != d.Generation {
		t.Error("a dry run changed the Deployment")
	}
	gen, err := fix.Apply(ctx, k8s.Client, p, false)
	if err != nil {
		t.Fatal(err)
	}
	live := k8s.Deployment(t, ns, "checkout")
	if got := live.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String(); got != "512Mi" || gen != live.Generation {
		t.Errorf("memory limit %s at generation %d, want 512Mi at %d", got, live.Generation, gen)
	}
	if mark, err := fix.Mark(ctx, k8s, tt); err != nil || mark != p.Mark {
		t.Errorf("mark %s (%v), want the plan's %s", mark, err, p.Mark)
	}

	name, params, err := fix.Undo(ctx, k8s, tt, p.Before)
	if want := set("app", map[string]string{"limits.memory": "256Mi"}); err != nil || name != resources.Name || !reflect.DeepEqual(params, want) {
		t.Fatalf("Undo = %s %v, %v; want %v", name, params, err, want)
	}
	back, err := fix.Plan(ctx, k8s, tt, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fix.Apply(ctx, k8s.Client, back, false); err != nil {
		t.Fatal(err)
	}
	if mark, err := fix.Mark(ctx, k8s, tt); err != nil || mark != p.Target.SpecHash {
		t.Errorf("after undoing, mark %s (%v), want the template from before the fix, %s", mark, err, p.Target.SpecHash)
	}
}
