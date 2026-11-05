package grpcapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	rollbackfix "github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/server/grpcapi"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

const agentAccount = "agents/triage"

// reviewer knows a token per ServiceAccount.
type reviewer map[string]string

func (r reviewer) Review(_ context.Context, token string) (string, error) {
	if account, ok := r[token]; ok {
		return account, nil
	}
	return "", grpcapi.ErrNotAServiceAccount
}

// external sets up an agent allowed in the incident's namespace, with changes
// to its configuration from edit, and a client that calls as it.
func (e *env) external(t *testing.T, edit ...func(*config.ExternalProposals)) themisv1.ProposalServiceClient {
	t.Helper()
	cfg := e.server.Config.Current()
	cfg.ExternalProposals = config.ExternalProposals{Sources: []config.ProposalSource{{
		ServiceAccount: agentAccount, Namespaces: []string{e.target.Namespace},
		Capabilities: []string{rollbackfix.Name}, PerMinute: 100,
	}}}
	for _, f := range edit {
		f(&cfg.ExternalProposals)
	}
	e.server.Config, e.server.Suggestions.Config, e.server.Investigations.Config = cfg, cfg, cfg
	e.server.Reviewer = reviewer{"agent-token": agentAccount, "stranger-token": "agents/stranger"}
	return e.clientWith(t, "agent-token")
}

func (e *env) clientWith(t *testing.T, tok string) themisv1.ProposalServiceClient {
	t.Helper()
	return themisv1.NewProposalServiceClient(dial(t, e.lis, grpc.WithPerRPCCredentials(token.GRPCCredentials(tok))))
}

// fromOutside is a valid suggestion for the leased investigation's incident.
func fromOutside(inv *themisv1.Investigation, clientID string) *themisv1.SubmitProposalRequest {
	req := rollback(inv, clientID)
	req.LeaseId = ""
	req.IncidentId = inv.GetIncidentId()
	req.Provenance = &themisv1.Provenance{Source: "claims-to-be-themis", Model: "other"}
	return req
}

func withWindow(x *config.ExternalProposals) { x.Window.Duration = time.Hour }

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("code = %s (%v), want %s", got, err, want)
	}
}

func TestExternal_OnlyAnAgentNamedInTheConfigurationMayCall(t *testing.T) {
	e := newEnv(t)
	e.external(t)
	ctx := t.Context()
	_, err := e.clientWith(t, "forged").ListIncidents(ctx, &themisv1.ListIncidentsRequest{})
	wantCode(t, err, codes.Unauthenticated)
	_, err = e.clientWith(t, "stranger-token").ListIncidents(ctx, &themisv1.ListIncidentsRequest{})
	wantCode(t, err, codes.PermissionDenied)
}

// The token gets an agent these calls, and nothing else the investigator has.
func TestExternal_AgentCannotCallTheInvestigatorsMethods(t *testing.T) {
	e := newEnv(t)
	e.external(t)
	conn := dial(t, e.lis, grpc.WithPerRPCCredentials(token.GRPCCredentials("agent-token")))
	_, err := themisv1.NewInvestigationServiceClient(conn).LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "x"})
	wantCode(t, err, codes.Unauthenticated)
	_, err = themisv1.NewEvidenceServiceClient(conn).CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{Template: "pod_states"})
	wantCode(t, err, codes.Unauthenticated)
}

func TestExternal_InvestigatorCannotListIncidents(t *testing.T) {
	e := newEnv(t)
	e.external(t)
	_, err := e.proposals.ListIncidents(t.Context(), &themisv1.ListIncidentsRequest{})
	wantCode(t, err, codes.PermissionDenied)
}

func TestExternal_ListsAndReadsOnlyWithinScope(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	list, err := agent.ListIncidents(t.Context(), &themisv1.ListIncidentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetIncidents()) != 1 || list.GetIncidents()[0].GetId() != inv.GetIncidentId() {
		t.Errorf("incidents = %v", list.GetIncidents())
	}
	got, err := agent.GetIncident(t.Context(), &themisv1.GetIncidentRequest{IncidentId: inv.GetIncidentId()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetEvidence()) == 0 || len(got.GetPredicates()) == 0 {
		t.Errorf("incident = %v", got)
	}

	outside := e.external(t, func(x *config.ExternalProposals) { x.Sources[0].Namespaces = []string{"elsewhere"} })
	list, err = outside.ListIncidents(t.Context(), &themisv1.ListIncidentsRequest{})
	if err != nil || len(list.GetIncidents()) != 0 {
		t.Errorf("incidents = %v, %v; want none", list, err)
	}
	_, err = outside.GetIncident(t.Context(), &themisv1.GetIncidentRequest{IncidentId: inv.GetIncidentId()})
	wantCode(t, err, codes.NotFound)
}

func TestExternal_WithholdsEvidenceAboveItsCeiling(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE evidence_items SET classification = 'sensitive' WHERE incident_id = $1 AND template = 'pod_states'`, inv.GetIncidentId()); err != nil {
		t.Fatal(err)
	}
	got, err := agent.GetIncident(t.Context(), &themisv1.GetIncidentRequest{IncidentId: inv.GetIncidentId()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetWithheld() == 0 {
		t.Error("nothing was withheld")
	}
	for _, it := range got.GetEvidence() {
		if it.GetClassification() != themisv1.DataClassification_DATA_CLASSIFICATION_INTERNAL {
			t.Errorf("an agent limited to internal got %s", it.GetTemplate())
		}
	}
	allowed := e.external(t, func(x *config.ExternalProposals) { x.Sources[0].Ceiling = "sensitive" })
	got, err = allowed.GetIncident(t.Context(), &themisv1.GetIncidentRequest{IncidentId: inv.GetIncidentId()})
	if err != nil || got.GetWithheld() != 0 {
		t.Errorf("withheld = %d, %v; want none", got.GetWithheld(), err)
	}
}

func reviewing(status authenticationv1.TokenReviewStatus, audiences *[]string) *fake.Clientset {
	c := fake.NewClientset()
	c.PrependReactor("create", "tokenreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		r := a.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview) //nolint:forcetypeassert // the reactor's resource
		*audiences = r.Spec.Audiences
		return true, &authenticationv1.TokenReview{Status: status}, nil
	})
	return c
}

func TestKubeTokenReviewer(t *testing.T) {
	sa := authenticationv1.UserInfo{Username: "system:serviceaccount:agents:triage"}
	tests := []struct {
		name   string
		status authenticationv1.TokenReviewStatus
		want   string
	}{
		{"a ServiceAccount", authenticationv1.TokenReviewStatus{Authenticated: true, User: sa, Audiences: []string{"themis"}}, "agents/triage"},
		{"not authenticated", authenticationv1.TokenReviewStatus{User: sa, Audiences: []string{"themis"}}, ""},
		{"another audience", authenticationv1.TokenReviewStatus{Authenticated: true, User: sa, Audiences: []string{"other"}}, ""},
		{"a person", authenticationv1.TokenReviewStatus{Authenticated: true, User: authenticationv1.UserInfo{Username: "alex"}, Audiences: []string{"themis"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []string
			got, err := grpcapi.KubeTokenReviewer{Clientset: reviewing(tt.status, &asked)}.Review(t.Context(), "tok")
			if tt.want == "" && !errors.Is(err, grpcapi.ErrNotAServiceAccount) {
				t.Errorf("err = %v", err)
			}
			if got != tt.want {
				t.Errorf("account = %q, want %q", got, tt.want)
			}
			if len(asked) != 1 || asked[0] != grpcapi.Audience {
				t.Errorf("asked for audiences %v", asked)
			}
		})
	}
}
