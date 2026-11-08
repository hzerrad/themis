package capability_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
)

// named is a fix that does nothing, for the registry.
type named string

func (n named) Name() string                                    { return string(n) }
func (named) Version() string                                   { return "1" }
func (named) Description() string                               { return "" }
func (named) Noun() string                                      { return "" }
func (named) Schema() json.RawMessage                           { return nil }
func (named) Validate(p map[string]any) (map[string]any, error) { return p, nil }
func (named) Plan(context.Context, client.Reader, incident.TargetRef, map[string]any) (capability.Plan, error) {
	return capability.Plan{}, nil
}

func (named) Apply(context.Context, client.Client, capability.Plan, bool) (int64, error) {
	return 0, nil
}

func (named) Undo(context.Context, client.Reader, incident.TargetRef, json.RawMessage) (string, map[string]any, error) {
	return "", nil, nil
}

func (named) Changes(json.RawMessage) ([]capability.Change, error) { return nil, nil }

func (named) Words(target string, _ map[string]any, _ json.RawMessage) capability.Words {
	return capability.Say("fix", capability.VerbChange, target, "")
}

func (named) Mark(context.Context, client.Reader, incident.TargetRef) (string, error) { return "", nil }

func (named) Check(context.Context, client.Reader, incident.TargetRef, string, json.RawMessage, time.Time) (capability.Reading, error) {
	return capability.Reading{}, nil
}

func TestRegistry(t *testing.T) {
	r := capability.NewRegistry(named("b.second"), named("a.first"))
	if got := r.Names(); !slices.Equal(got, []string{"a.first", "b.second"}) {
		t.Errorf("Names = %v", got)
	}
	if f, ok := r["b.second"]; !ok || f.Name() != "b.second" {
		t.Errorf("lookup = %v, %v", f, ok)
	}
	// Only a fix that writes a pod template works out a record from two.
	if _, err := r.Record("b.second", nil, nil, nil); err == nil {
		t.Error("Record worked out a record for a fix that writes no template")
	}
}

func TestFields_MarksAnUndo(t *testing.T) {
	plan := capability.Plan{Params: map[string]any{"revision": int64(1)}}
	f := capability.Fields("i1", named("x.fix"), plan, "a1", "p", "pd")
	if f.Capability != "x.fix" || f.CapabilityVersion != "1" || f.Params["restores"] != "a1" || f.Params["revision"] != int64(1) {
		t.Errorf("fields = %+v", f)
	}
	if _, ok := capability.Fields("i1", named("x.fix"), plan, "", "p", "pd").Params["restores"]; ok {
		t.Error("a fix of its own is marked as an undo")
	}
}

// The registry names a restore as one, whatever the fix, a fix it has in
// that fix's words, and one it doesn't as a change.
func TestRegistry_Words(t *testing.T) {
	r := capability.NewRegistry(named("x.fix"))
	for name, tc := range map[string]struct {
		fix    string
		params map[string]any
		want   capability.Words
	}{
		"a restore": {"x.fix", map[string]any{"restores": "a1"}, capability.Words{
			Noun: "restore", Act: "Restore the previous state of payments",
			Doing: "Restoring the previous state of payments.", Done: "Restored the previous state of payments.",
		}},
		"a fix it has":     {"x.fix", nil, capability.Say("fix", capability.VerbChange, "payments", "")},
		"a fix it doesn't": {"y.fix", nil, capability.Words{Noun: "change", Act: "Apply this change to payments", Doing: "Applying the change to payments.", Done: "Changed payments."}},
	} {
		if got := r.Words(tc.fix, "payments", tc.params, nil); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
	// A name that ends in s takes only the apostrophe.
	if got := capability.Of("payments", []string{"app", "worker"}, false, "CPU limits"); got != "the CPU limits of payments' app and worker containers" {
		t.Errorf("Of = %q", got)
	}
}

func TestRefusal(t *testing.T) {
	var err error = &capability.Refusal{Reason: "Revision 3 no longer exists."}
	if err.Error() != "Revision 3 no longer exists." {
		t.Errorf("Error() = %q", err.Error())
	}
}
