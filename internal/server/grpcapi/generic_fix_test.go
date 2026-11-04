package grpcapi_test

import (
	"encoding/json"
	"testing"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/config"
)

// restart is a fix Themis doesn't ship with; suggesters only read its description.
type restart struct{ capability.Capability }

func (restart) Name() string            { return "test.restart" }
func (restart) Description() string     { return "Restarts the workload's pods." }
func (restart) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// A suggester learns each fix it may suggest from the fixes the server was
// given: what it does and its schema.
func TestGenericFix_SuggestersLearnTheFixes(t *testing.T) {
	p := profile()
	p.Capabilities = []string{"test.restart"}
	e := newEnvWith(t, p)
	e.server.Fixes = capability.NewRegistry(restart{})
	inv := e.lease(t)
	check := func(who string, caps []*themisv1.Capability) {
		t.Helper()
		if len(caps) != 1 || caps[0].GetName() != "test.restart" || caps[0].GetDescription() != "Restarts the workload's pods." {
			t.Fatalf("%s learns %v", who, caps)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(caps[0].GetParamsSchema()), &schema); err != nil || schema["type"] != "object" {
			t.Errorf("%s's schema = %q, %v", who, caps[0].GetParamsSchema(), err)
		}
	}
	check("the investigator", inv.GetCapabilities())
	agent := e.external(t, withWindow, func(x *config.ExternalProposals) { x.Sources[0].Capabilities = p.Capabilities })
	got, err := agent.GetIncident(t.Context(), &themisv1.GetIncidentRequest{IncidentId: inv.GetIncidentId()})
	if err != nil {
		t.Fatal(err)
	}
	check("an outside agent", got.GetCapabilities())
}
