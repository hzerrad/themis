package grpcserver_test

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

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/grpcserver"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

const agentAccount = "agents/triage"

// reviewer knows a token per ServiceAccount.
type reviewer map[string]string

func (r reviewer) Review(_ context.Context, token string) (string, error) {
	if account, ok := r[token]; ok {
		return account, nil
	}
	return "", grpcserver.ErrNotAServiceAccount
}

// external sets up an agent allowed in the incident's namespace, with changes
// to its configuration from edit, and a client that calls as it.
func (e *env) external(t *testing.T, edit ...func(*config.ExternalProposals)) themisv1.ProposalServiceClient {
	t.Helper()
	cfg := e.server.Config.Current()
	cfg.ExternalProposals = config.ExternalProposals{Sources: []config.ProposalSource{{
		ServiceAccount: agentAccount, Namespaces: []string{e.target.Namespace},
		Capabilities: []string{grpcserver.CapabilityRollback}, PerMinute: 100,
	}}}
	for _, f := range edit {
		f(&cfg.ExternalProposals)
	}
	e.server.Config = cfg
	e.server.Reviewer = reviewer{"agent-token": agentAccount, "stranger-token": "agents/stranger"}
	return e.clientWith(t, "agent-token")
}

func (e *env) clientWith(t *testing.T, tok string) themisv1.ProposalServiceClient {
	t.Helper()
	return themisv1.NewProposalServiceClient(dial(t, e.lis, grpc.WithPerRPCCredentials(grpcserver.BearerToken(tok))))
}

// suggestion is a valid suggestion for the leased investigation's incident.
func suggestion(inv *themisv1.Investigation, clientID string) *themisv1.SubmitProposalRequest {
	req := rollback(inv, clientID)
	req.LeaseId = ""
	req.IncidentId = inv.GetIncidentId()
	req.Provenance = &themisv1.Provenance{Source: "claims-to-be-themis", Model: "other"}
	return req
}

func (e *env) abstain(t *testing.T, inv *themisv1.Investigation) {
	t.Helper()
	_, err := e.investigations.CompleteInvestigation(t.Context(), &themisv1.CompleteInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_INCONCLUSIVE, Summary: "can't tell",
	})
	if err != nil {
		t.Fatalf("CompleteInvestigation: %v", err)
	}
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
	conn := dial(t, e.lis, grpc.WithPerRPCCredentials(grpcserver.BearerToken("agent-token")))
	_, err := themisv1.NewInvestigationServiceClient(conn).LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "x"})
	wantCode(t, err, codes.Unauthenticated)
	_, err = themisv1.NewEvidenceServiceClient(conn).CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{Template: "pod_states"})
	wantCode(t, err, codes.Unauthenticated)
}

func TestExternal_AgentCannotUseALease(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	_, err := agent.SubmitProposal(t.Context(), rollback(inv, "c1"))
	wantCode(t, err, codes.PermissionDenied)
}

func TestExternal_InvestigatorCannotNameAnIncident(t *testing.T) {
	e := newEnv(t)
	e.external(t)
	inv := e.lease(t)
	_, err := e.proposals.SubmitProposal(t.Context(), suggestion(inv, "c1"))
	wantCode(t, err, codes.Unauthenticated)
	_, err = e.proposals.ListIncidents(t.Context(), &themisv1.ListIncidentsRequest{})
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

// Themis couldn't decide, and an agent suggests a fix in the time left.
func TestExternal_SuggestionAfterThemisCouldntDecideGoesToGovernance(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t, withWindow)
	inv := e.lease(t)
	e.abstain(t, inv)
	if got := e.state(t, inv.GetIncidentId()); got != incident.Inconclusive {
		t.Fatalf("state = %s, want it open for a suggestion", got)
	}
	resp, err := agent.SubmitProposal(t.Context(), suggestion(inv, "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED {
		t.Fatalf("response = %v", resp)
	}
	if got := e.state(t, inv.GetIncidentId()); got != incident.Diagnosed {
		t.Errorf("state = %s, want diagnosed", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = 'govern'`); n != 1 {
		t.Errorf("%d govern jobs, want 1", n)
	}
}

// Who suggested it comes from the token. What it claims is kept apart.
func TestExternal_RecordsProvenIdentityNotTheClaim(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t, withWindow)
	inv := e.lease(t)
	resp, err := agent.SubmitProposal(t.Context(), suggestion(inv, "c1"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.store.Proposal(t.Context(), e.store.DB(), resp.GetProposalId())
	if err != nil {
		t.Fatal(err)
	}
	if !p.Authenticated || p.Provenance["source"] != "serviceaccount:"+agentAccount || p.Provenance["claimedSource"] != "claims-to-be-themis" || p.LeaseID != "" {
		t.Errorf("proposal = %+v", p)
	}
}

func TestExternal_SuggestionDuringTheInvestigationIsUsedIfThemisHasNone(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	if _, err := agent.SubmitProposal(t.Context(), suggestion(inv, "c1")); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, inv.GetIncidentId()); got != incident.Investigating {
		t.Fatalf("state = %s, want the investigation undisturbed", got)
	}
	e.abstain(t, inv)
	if got := e.state(t, inv.GetIncidentId()); got != incident.Diagnosed {
		t.Errorf("state = %s, want diagnosed", got)
	}
}

func TestExternal_InvestigatorsOwnFixComesFirst(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	if _, err := agent.SubmitProposal(t.Context(), suggestion(inv, "theirs")); err != nil {
		t.Fatal(err)
	}
	own := e.submit(t, rollback(inv, "ours"))
	_, err := e.investigations.CompleteInvestigation(t.Context(), &themisv1.CompleteInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_PROPOSED, Summary: "found it",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = 'govern' AND args ->> 'proposalID' = $1`, own.GetProposalId()); n != 1 {
		t.Errorf("%d govern jobs for the investigator's fix, want 1", n)
	}
}

func TestExternal_RefusedSuggestionsAreKeptWithWhy(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*config.ExternalProposals)
		change func(*themisv1.SubmitProposalRequest)
		reason themisv1.RejectionReason
	}{
		{"a capability it may not suggest", func(x *config.ExternalProposals) { x.Sources[0].Capabilities = []string{"something.else"} }, nil, themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED},
		{"evidence never collected", nil, func(r *themisv1.SubmitProposalRequest) {
			r.CitedEvidenceIds = []string{"00000000-0000-7000-8000-000000000000"}
		}, themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE},
		{"a bad revision", nil, func(r *themisv1.SubmitProposalRequest) {
			r.Capability = &themisv1.SubmitProposalRequest_RollbackToRevision{RollbackToRevision: &themisv1.RollbackToRevisionParams{Revision: -1}}
		}, themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			agent := e.external(t, func(x *config.ExternalProposals) {
				if tt.edit != nil {
					tt.edit(x)
				}
			})
			inv := e.lease(t)
			req := suggestion(inv, "c1")
			if tt.change != nil {
				tt.change(req)
			}
			resp, err := agent.SubmitProposal(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			assertRejected(t, resp, tt.reason)
		})
	}
}

func TestExternal_AnotherTargetIsASafetyEvent(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	req := suggestion(inv, "c1")
	req.Target.Name = "coredns"
	resp, err := agent.SubmitProposal(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertRejected(t, resp, themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH)
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM safety_events WHERE proposal_id = $1`, resp.GetProposalId()); n != 1 {
		t.Errorf("%d safety events, want 1", n)
	}
}

func TestExternal_ClosedIncidentTakesNoSuggestion(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	e.server.Config = func() config.Config {
		cfg := e.server.Config.Current()
		cfg.ExternalProposals.Sources = nil
		return cfg
	}()
	e.abstain(t, inv) // with no source, nothing keeps it open
	e.external(t)
	resp, err := agent.SubmitProposal(t.Context(), suggestion(inv, "c1"))
	if err != nil {
		t.Fatal(err)
	}
	assertRejected(t, resp, themisv1.RejectionReason_REJECTION_REASON_INCIDENT_CLOSED)
	if got := e.state(t, inv.GetIncidentId()); got != incident.ClosedNoAction {
		t.Errorf("state = %s", got)
	}
}

func TestExternal_SameSuggestionIsStoredOnce(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	first, err := agent.SubmitProposal(t.Context(), suggestion(inv, "c1"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := agent.SubmitProposal(t.Context(), suggestion(inv, "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if first.GetProposalId() != again.GetProposalId() {
		t.Errorf("ids = %s, %s; want one proposal", first.GetProposalId(), again.GetProposalId())
	}
}

func TestExternal_LimitsHowOftenASourceSuggests(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t, func(x *config.ExternalProposals) { x.Sources[0].PerMinute = 1 })
	inv := e.lease(t)
	if _, err := agent.SubmitProposal(t.Context(), suggestion(inv, "c1")); err != nil {
		t.Fatal(err)
	}
	_, err := agent.SubmitProposal(t.Context(), suggestion(inv, "c2"))
	wantCode(t, err, codes.ResourceExhausted)
}

func TestExternal_LimitsHowManyASourceLeavesOnAnIncident(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	for i := range 20 {
		if _, err := agent.SubmitProposal(t.Context(), suggestion(inv, string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	_, err := agent.SubmitProposal(t.Context(), suggestion(inv, "one-too-many"))
	wantCode(t, err, codes.ResourceExhausted)
}

func TestExternal_RefusesAnOverlongSuggestion(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	req := suggestion(e.lease(t), "c1")
	req.Rationale = string(make([]byte, 5000))
	_, err := agent.SubmitProposal(t.Context(), req)
	wantCode(t, err, codes.InvalidArgument)
}

func TestSubmitFromPerson_RecordsTheTokenAndPassesTheSameChecks(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	resp, err := e.server.SubmitFromPerson(t.Context(), suggestion(inv, "c1"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.store.Proposal(t.Context(), e.store.DB(), resp.GetProposalId())
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != store.ProposalAccepted || p.Provenance["source"] != grpcserver.PersonSource {
		t.Errorf("proposal = %+v", p)
	}
	req := suggestion(inv, "c2")
	req.Target.Name = "coredns"
	bad, err := e.server.SubmitFromPerson(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertRejected(t, bad, themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH)
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
			got, err := grpcserver.KubeTokenReviewer{Clientset: reviewing(tt.status, &asked)}.Review(t.Context(), "tok")
			if tt.want == "" && !errors.Is(err, grpcserver.ErrNotAServiceAccount) {
				t.Errorf("err = %v", err)
			}
			if got != tt.want {
				t.Errorf("account = %q, want %q", got, tt.want)
			}
			if len(asked) != 1 || asked[0] != grpcserver.Audience {
				t.Errorf("asked for audiences %v", asked)
			}
		})
	}
}

// Outside its scope, an incident looks just like one that doesn't exist, and
// a source can't leave anything on it.
func TestExternal_IncidentOutOfScopeLooksMissing(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t, func(x *config.ExternalProposals) { x.Sources[0].Namespaces = []string{"elsewhere"} })
	inv := e.lease(t)
	_, outside := agent.SubmitProposal(t.Context(), suggestion(inv, "c1"))
	wantCode(t, outside, codes.NotFound)
	missing := suggestion(inv, "c2")
	missing.IncidentId = "00000000-0000-7000-8000-000000000000"
	_, absent := agent.SubmitProposal(t.Context(), missing)
	if status.Convert(outside).Message() != status.Convert(absent).Message() {
		t.Errorf("answers differ: %q, %q", status.Convert(outside).Message(), status.Convert(absent).Message())
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM proposals WHERE incident_id = $1`, inv.GetIncidentId()); n != 0 {
		t.Errorf("%d proposals stored, want none", n)
	}
}

func (e *env) failEveryAttempt(t *testing.T, agent themisv1.ProposalServiceClient) (string, *themisv1.SubmitProposalResponse) {
	t.Helper()
	var (
		id   string
		resp *themisv1.SubmitProposalResponse
	)
	for i := range store.MaxInvestigationAttempts {
		inv := e.lease(t)
		id = inv.GetIncidentId()
		if i == 0 {
			var err error
			if resp, err = agent.SubmitProposal(t.Context(), suggestion(inv, "c1")); err != nil {
				t.Fatal(err)
			}
		}
		_, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{
			LeaseId: inv.GetLeaseId(), Reason: themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE,
		})
		if err != nil {
			t.Fatal(err)
		}
		e.clock.now = e.clock.now.Add(10 * time.Minute)
	}
	return id, resp
}

// An agent was told its suggestion was accepted: the investigator failing
// doesn't lose it.
func TestExternal_SuggestionIsUsedWhenTheInvestigationFails(t *testing.T) {
	e := newEnv(t)
	id, resp := e.failEveryAttempt(t, e.external(t))
	if got := e.state(t, id); got != incident.Diagnosed {
		t.Errorf("state = %s, want diagnosed", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = 'govern' AND args ->> 'proposalID' = $1`, resp.GetProposalId()); n != 1 {
		t.Errorf("%d govern jobs, want 1", n)
	}
}

func TestExternal_SuggestionIsUsedWhenTheInvestigatorStopsResponding(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	var (
		id   string
		resp *themisv1.SubmitProposalResponse
	)
	for i := range store.MaxInvestigationAttempts {
		inv := e.lease(t)
		id = inv.GetIncidentId()
		if i == 0 {
			var err error
			if resp, err = agent.SubmitProposal(t.Context(), suggestion(inv, "c1")); err != nil {
				t.Fatal(err)
			}
		}
		e.clock.now = e.clock.now.Add(10 * time.Minute)
	}
	if _, err := e.investigations.LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "w"}); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, id); got != incident.Diagnosed {
		t.Errorf("state = %s, want diagnosed", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = 'govern' AND args ->> 'proposalID' = $1`, resp.GetProposalId()); n != 1 {
		t.Errorf("%d govern jobs, want 1", n)
	}
}

// With no suggestion to fall back on, a failed investigation still escalates:
// the investigator's own fix, from a lease that failed, isn't used.
func TestExternal_FailureWithOnlyTheInvestigatorsFixEscalates(t *testing.T) {
	e := newEnv(t)
	e.external(t)
	var id string
	for i := range store.MaxInvestigationAttempts {
		inv := e.lease(t)
		id = inv.GetIncidentId()
		if i == 0 {
			e.submit(t, rollback(inv, "ours"))
		}
		if _, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{
			LeaseId: inv.GetLeaseId(), Reason: themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE,
		}); err != nil {
			t.Fatal(err)
		}
		e.clock.now = e.clock.now.Add(10 * time.Minute)
	}
	if got := e.state(t, id); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
}
