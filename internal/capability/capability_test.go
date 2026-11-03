package capability_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/capability"
	"github.com/hzerrad/themis/internal/incident"
)

// named is a fix that does nothing, for the registry.
type named string

func (n named) Name() string                                    { return string(n) }
func (named) Version() string                                   { return "1" }
func (named) Description() string                               { return "" }
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

func (named) Changes(json.RawMessage, json.RawMessage) ([]capability.Change, error) { return nil, nil }

func TestRegistry(t *testing.T) {
	r := capability.NewRegistry(named("b.second"), named("a.first"))
	if got := r.Names(); !slices.Equal(got, []string{"a.first", "b.second"}) {
		t.Errorf("Names = %v", got)
	}
	if f, ok := r["b.second"]; !ok || f.Name() != "b.second" {
		t.Errorf("lookup = %v, %v", f, ok)
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

func TestRefusal(t *testing.T) {
	var err error = &capability.Refusal{Reason: "Revision 3 no longer exists."}
	if err.Error() != "Revision 3 no longer exists." {
		t.Errorf("Error() = %q", err.Error())
	}
}
