package approval_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hzerrad/themis/internal/approval"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

const token = "admin-s3cret"

var now = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

type env struct {
	store   *store.Store
	pool    *pgxpool.Pool
	handler http.Handler
	clock   time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{store: s, pool: pool, clock: now}
	mux := http.NewServeMux()
	(&approval.API{Store: s, TokenFile: file, Now: func() time.Time { return e.clock }}).Register(mux)
	e.handler = mux
	return e
}

var target = incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid-checkout"}

// awaiting creates an incident whose action awaits approval.
func (e *env) awaiting(t *testing.T) store.Action {
	t.Helper()
	ctx := t.Context()
	var a store.Action
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		id, _, err := e.store.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{Fingerprint: "fp", Profile: "rollout-regression", Target: target, Payload: []byte(`{}`)})
		if err != nil {
			return err
		}
		path := []incident.State{incident.Received, incident.Investigating, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval}
		for i := 1; i < len(path); i++ {
			if err := e.store.Transition(ctx, tx, store.TransitionArgs{ID: id, From: path[i-1], To: path[i], Reason: "test", Actor: "test"}); err != nil {
				return err
			}
		}
		if _, err := e.store.CreateInvestigation(ctx, tx, id, nil, nil); err != nil {
			return err
		}
		p, _, err := e.store.InsertProposal(ctx, tx, store.Proposal{
			IncidentID: id, LeaseID: "0192f0c1-0000-7000-8000-000000000001", ClientProposalID: "p",
			Capability: "deployment.rollback-to-revision", CapabilityVersion: "1", Target: target,
			Params: map[string]any{"revision": float64(1)}, Rationale: "revision 2 is OOMKilled", Status: store.ProposalAccepted,
		})
		if err != nil {
			return err
		}
		expires := now.Add(30 * time.Minute)
		a = store.Action{
			IncidentID: id, ProposalID: p.ID, Capability: "deployment.rollback-to-revision", CapabilityVersion: "1",
			Target:   digest.TargetIdentity{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: target.UID, Generation: 2, SpecHash: "sha256:s"},
			Params:   map[string]any{"revision": int64(1), "templateHash": "sha256:t"},
			Decision: "require_approval", PolicyID: "approve", PolicyDigest: "sha256:p", Constraints: map[string]any{},
			BlastRadius: map[string]any{"replicas": int64(3)}, Risk: "medium",
			PreState: []byte(`{"spec":{"containers":[{"name":"app","image":"busybox","env":[{"name":"MODE","value":"leak"},{"name":"DB_PASSWORD","value":"s3cret-now"}]}]}}`),
			Desired:  []byte(`{"spec":{"containers":[{"name":"app","image":"busybox","env":[{"name":"MODE","value":"healthy"},{"name":"DB_PASSWORD","value":"s3cret-before"}]}]}}`),
			State:    store.ActionAwaitingApproval, ExpiresAt: &expires,
		}
		a.Digest, err = digest.Compute(a.Fields())
		if err != nil {
			return err
		}
		a.ID, err = e.store.InsertAction(ctx, tx, a)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *env) do(t *testing.T, method, path, auth string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, &b)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) approve(t *testing.T, a store.Action, dg string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/actions/"+a.ID+"/approve", token, map[string]string{"digest": dg})
}

func (e *env) state(t *testing.T, id string) incident.State {
	t.Helper()
	in, err := e.store.Incident(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

func TestApprove_NoToken_401(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	for _, auth := range []string{"", "guess"} {
		if rec := e.do(t, http.MethodPost, "/v1/actions/"+a.ID+"/approve", auth, map[string]string{"digest": a.Digest}); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", auth, rec.Code)
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/incidents", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("list without token: status = %d, want 401", rec.Code)
	}
}

func TestApprove_BindsDigest(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if rec := e.approve(t, a, "sha256:what-the-page-did-not-show"); rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if got := e.state(t, a.IncidentID); got != incident.AwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval", got)
	}
}

func TestApprove_Expired(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	e.clock = now.Add(time.Hour)
	if rec := e.approve(t, a, a.Digest); rec.Code != http.StatusGone {
		t.Errorf("status = %d, want 410", rec.Code)
	}
}

func TestApprove_Twice_SecondFails(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if rec := e.approve(t, a, a.Digest); rec.Code != http.StatusOK {
		t.Fatalf("first approval: status = %d: %s", rec.Code, rec.Body)
	}
	if rec := e.approve(t, a, a.Digest); rec.Code != http.StatusConflict {
		t.Errorf("second approval: status = %d, want 409", rec.Code)
	}
}

func TestApprove_TransitionsToAuthorizedAndEnqueuesExecute(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if rec := e.approve(t, a, a.Digest); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := e.state(t, a.IncidentID); got != incident.Authorized {
		t.Errorf("state = %s, want authorized", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'actionID' = $2`, governance.ExecuteArgs{}.Kind(), a.ID); n != 1 {
		t.Errorf("%d execute jobs, want 1", n)
	}
}

func TestApprove_UnknownAction_404(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPost, "/v1/actions/0192f0c1-0000-7000-8000-0000000000ff/approve", token, map[string]string{"digest": "x"}); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDeny_ClosesWithoutAction(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if rec := e.do(t, http.MethodPost, "/v1/actions/"+a.ID+"/deny", token, map[string]string{"reason": "not during the sale"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := e.state(t, a.IncidentID); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
	if rec := e.approve(t, a, a.Digest); rec.Code != http.StatusConflict {
		t.Errorf("approval after denial: status = %d, want 409", rec.Code)
	}
}

func TestIncidentDetail_SeparatesProposalAndDerivedAction(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	rec := e.do(t, http.MethodGet, "/v1/incidents/"+a.IncidentID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var detail struct {
		Incident    struct{ ID, State string }
		Transitions []struct{ From, To string }
		Proposals   []struct{ ID, Rationale string }
		Actions     []struct {
			ID, Digest, State, DecisionReason string
			ProposalID                        string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Incident.ID != a.IncidentID || len(detail.Transitions) != 4 {
		t.Errorf("incident = %+v with %d transitions", detail.Incident, len(detail.Transitions))
	}
	if len(detail.Proposals) != 1 || len(detail.Actions) != 1 || detail.Actions[0].Digest != a.Digest || detail.Actions[0].ProposalID != detail.Proposals[0].ID {
		t.Errorf("proposals = %+v, actions = %+v", detail.Proposals, detail.Actions)
	}
}

func TestIncidentDetail_EvidenceIsJSON(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		return e.store.InsertEvidence(t.Context(), tx, a.IncidentID, "", nil, evidence.Item{
			ID: "0192f0c1-0000-7000-8000-00000000000a", Template: evidence.PodStates, Data: []byte(`{"pods":[]}`),
			CollectedAt: now, Classification: evidence.ClassificationInternal,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, http.MethodGet, "/v1/incidents/"+a.IncidentID, token, nil)
	var detail struct {
		Evidence []struct{ Data map[string]any }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || len(detail.Evidence) != 1 || detail.Evidence[0].Data["pods"] == nil {
		t.Errorf("evidence = %s (%v)", rec.Body, err)
	}
}

func TestListIncidents(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	rec := e.do(t, http.MethodGet, "/v1/incidents", token, nil)
	var list []struct{ ID, State string }
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != a.IncidentID || list[0].State != "awaiting_approval" {
		t.Errorf("list = %+v", list)
	}
}

// Nobody answered: unlike a denial, the incident is still live.
func TestExpire_Escalates(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	w := &approval.ExpireWorker{Store: e.store, Now: func() time.Time { return now.Add(time.Hour) }}
	if err := w.Expire(t.Context()); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if got := e.state(t, a.IncidentID); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
}

// The detail shows what MODE changes to, never a secret.
func TestIncidentDetail_ShowsChangesNotTemplates(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	rec := e.do(t, http.MethodGet, "/v1/incidents/"+a.IncidentID, token, nil)
	body := rec.Body.String()
	if strings.Contains(body, "s3cret") {
		t.Error("the detail contains a secret")
	}
	if !strings.Contains(body, `"field":"env MODE","before":"leak","after":"healthy"`) {
		t.Errorf("the detail does not show how MODE changes: %s", body)
	}
}

// One incident that moved on must not stop the sweep for every other one.
func TestExpire_ToleratesAnIncidentThatMovedOn(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE incidents SET state = 'self_resolved' WHERE id = $1`, a.IncidentID); err != nil {
		t.Fatal(err)
	}
	w := &approval.ExpireWorker{Store: e.store, Now: func() time.Time { return now.Add(time.Hour) }}
	if err := w.Expire(t.Context()); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if got, _ := e.store.Action(t.Context(), e.store.DB(), a.ID); got.State != store.ActionExpired {
		t.Errorf("action = %s, want expired", got.State)
	}
}
