package grpcapi_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
	"github.com/hzerrad/themis/internal/capability"
	"github.com/hzerrad/themis/internal/config"
)

// generic is req in the generic form: the fix by name, with its parameters.
func generic(t *testing.T, req *themisv1.SubmitProposalRequest, params map[string]any) *themisv1.SubmitProposalRequest {
	t.Helper()
	s, err := structpb.NewStruct(params)
	if err != nil {
		t.Fatal(err)
	}
	req.Capability = nil
	req.CapabilityName = "deployment.rollback-to-revision"
	req.Params = s
	return req
}

// The generic form stores exactly what the older rollback form does, so
// everything after it, the approval's digest included, is the same.
func TestGenericFix_StoresWhatTheRollbackFormDoes(t *testing.T) {
	older := newEnv(t)
	inv := older.lease(t)
	want, err := older.store.Proposal(t.Context(), older.store.DB(), older.submit(t, rollback(inv, "c1")).GetProposalId())
	if err != nil {
		t.Fatal(err)
	}

	e := newEnv(t)
	inv = e.lease(t)
	resp := e.submit(t, generic(t, rollback(inv, "c1"), map[string]any{"revision": 1}))
	if resp.GetStatus() != themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED {
		t.Fatalf("response = %v", resp)
	}
	got, err := e.store.Proposal(t.Context(), e.store.DB(), resp.GetProposalId())
	if err != nil {
		t.Fatal(err)
	}
	if got.Capability != want.Capability || got.CapabilityVersion != want.CapabilityVersion || !reflect.DeepEqual(got.Params, want.Params) {
		t.Errorf("stored %s v%s %#v; the rollback form stores %s v%s %#v",
			got.Capability, got.CapabilityVersion, got.Params, want.Capability, want.CapabilityVersion, want.Params)
	}
}

func TestGenericFix_Refuses(t *testing.T) {
	for name, tc := range map[string]struct {
		params map[string]any
		says   string
	}{
		"no revision":           {map[string]any{}, "positive whole number"},
		"a fractional revision": {map[string]any{"revision": 1.5}, "positive whole number"},
		"an unknown parameter":  {map[string]any{"revision": 1, "force": true}, "only revision"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			inv := e.lease(t)
			resp := e.submit(t, generic(t, rollback(inv, "c1"), tc.params))
			if resp.GetStatus() != themisv1.ProposalStatus_PROPOSAL_STATUS_REJECTED ||
				resp.GetRejectionReason() != themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS || !strings.Contains(resp.GetMessage(), tc.says) {
				t.Errorf("response = %v, want rejected for its parameters saying %q", resp, tc.says)
			}
		})
	}
}

func TestGenericFix_NotBothForms(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := generic(t, rollback(inv, "c1"), map[string]any{"revision": 1})
	req.Capability = &themisv1.SubmitProposalRequest_RollbackToRevision{RollbackToRevision: &themisv1.RollbackToRevisionParams{Revision: 1}}
	_, err := e.proposals.SubmitProposal(t.Context(), req)
	wantCode(t, err, codes.InvalidArgument)
}

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
