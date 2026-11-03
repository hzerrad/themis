package fix_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/fix"
	"github.com/hzerrad/themis/internal/incident"
)

// named is a fix that does nothing, for the registry.
type named string

func (n named) Name() string  { return string(n) }
func (named) Version() string { return "1" }
func (named) Plan(context.Context, client.Reader, incident.TargetRef, map[string]any) (fix.Plan, error) {
	return fix.Plan{}, nil
}
func (named) Apply(context.Context, client.Client, fix.Plan, bool) (int64, error) { return 0, nil }
func (named) Undo(context.Context, client.Reader, incident.TargetRef, json.RawMessage) (string, map[string]any, error) {
	return "", nil, nil
}
func (named) Changes(json.RawMessage, json.RawMessage) ([]fix.Change, error) { return nil, nil }

func TestRegistry(t *testing.T) {
	r := fix.NewRegistry(named("b.second"), named("a.first"))
	if got := r.Names(); !slices.Equal(got, []string{"a.first", "b.second"}) {
		t.Errorf("Names = %v", got)
	}
	if f, ok := r["b.second"]; !ok || f.Name() != "b.second" {
		t.Errorf("lookup = %v, %v", f, ok)
	}
}

func TestFields_MarksAnUndo(t *testing.T) {
	plan := fix.Plan{Params: map[string]any{"revision": int64(1)}}
	f := fix.Fields("i1", named("x.fix"), plan, "a1", "p", "pd")
	if f.Capability != "x.fix" || f.CapabilityVersion != "1" || f.Params["restores"] != "a1" || f.Params["revision"] != int64(1) {
		t.Errorf("fields = %+v", f)
	}
	if _, ok := fix.Fields("i1", named("x.fix"), plan, "", "p", "pd").Params["restores"]; ok {
		t.Error("a fix of its own is marked as an undo")
	}
}

func TestRefusal(t *testing.T) {
	var err error = &fix.Refusal{Reason: "Revision 3 no longer exists."}
	if err.Error() != "Revision 3 no longer exists." {
		t.Errorf("Error() = %q", err.Error())
	}
}
