package httpapi_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability/probes"
	"github.com/hzerrad/themis/internal/catalog/capability/resources"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/hint"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

const token = "admin-s3cret"

var now = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

type env struct {
	logs    bytes.Buffer
	store   *store.Store
	pool    *pgxpool.Pool
	handler http.Handler
	clock   time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith is newEnv, with what with sets on the API first.
func newEnvWith(t *testing.T, with func(a *httpapi.API)) *env {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{store: s, pool: pool, clock: now}
	mux := http.NewServeMux()
	api := &httpapi.API{
		Store: s, TokenFile: file, Fixes: catalog.Fixes(), Now: func() time.Time { return e.clock },
		RootCauses: catalog.RootCauses{"rollout-regression": {{Name: "new_version_broken", Description: "a recent rollout brought a version that fails"}}},
		Logger:     telemetry.NewLogger(&e.logs, "core", slog.LevelDebug),
	}
	if with != nil {
		with(api)
	}
	api.Register(mux)
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
		err = e.store.TransitionPath(ctx, tx, id, "test", "test", incident.Received, incident.Investigating, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval)
		if err != nil {
			return err
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
		record, err := catalog.Fixes().Record(rollback.Name, map[string]any{"revision": int64(1)},
			[]byte(`{"spec":{"containers":[{"name":"app","image":"busybox","env":[{"name":"MODE","value":"leak"},{"name":"DB_PASSWORD","value":"s3cret-now"}]}]}}`),
			[]byte(`{"spec":{"containers":[{"name":"app","image":"busybox","env":[{"name":"MODE","value":"healthy"},{"name":"DB_PASSWORD","value":"s3cret-before"}]}]}}`))
		if err != nil {
			return err
		}
		a = store.Action{
			IncidentID: id, ProposalID: p.ID, Capability: "deployment.rollback-to-revision", CapabilityVersion: "1",
			Target:   digest.TargetIdentity{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: target.UID, Generation: 2, SpecHash: "sha256:s"},
			Params:   map[string]any{"revision": int64(1), "templateHash": "sha256:t"},
			Decision: "require_approval", PolicyID: "approve", PolicyDigest: "sha256:p", Constraints: map[string]any{},
			BlastRadius: map[string]any{"replicas": int64(3)}, Risk: "medium",
			Record: record,
			State:  store.ActionAwaitingApproval, ExpiresAt: &expires,
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

// The API answers an approver with what became of their answer: what that
// does to the incident is governance's, and tested there.
func TestApprove_Status(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if rec := e.approve(t, a, "sha256:what-the-page-did-not-show"); rec.Code != http.StatusConflict {
		t.Errorf("another digest: status = %d, want 409", rec.Code)
	}
	if rec := e.approve(t, a, a.Digest); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if rec := e.approve(t, a, a.Digest); rec.Code != http.StatusConflict {
		t.Errorf("second approval: status = %d, want 409", rec.Code)
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

// A fix put to a person before the install stopped Themis applying it isn't
// approved, and the refusal says why: the executor wouldn't apply it.
func TestApprove_FixThemisCantApplyNow(t *testing.T) {
	e := newEnvWith(t, func(a *httpapi.API) { a.Reach = &governance.Reach{} })
	a := e.awaiting(t)
	rec := e.approve(t, a, a.Digest)
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusConflict || got != "Refused. The executor is off." {
		t.Errorf("status = %d, body = %q; want 409 and why", rec.Code, got)
	}
}

func TestDeny_Status(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if rec := e.do(t, http.MethodPost, "/v1/actions/"+a.ID+"/deny", token, map[string]string{"reason": "not during the sale"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if rec := e.approve(t, a, a.Digest); rec.Code != http.StatusConflict {
		t.Errorf("approval after denial: status = %d, want 409", rec.Code)
	}
	if rec := e.do(t, http.MethodPost, "/v1/actions/"+a.ID+"/deny", token, map[string]string{}); rec.Code != http.StatusConflict {
		t.Errorf("second denial: status = %d, want 409", rec.Code)
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
	// The detail shows what MODE changes to, never a secret.
	body := rec.Body.String()
	if strings.Contains(body, "s3cret") {
		t.Error("the detail contains a secret")
	}
	if !strings.Contains(body, `"field":"env MODE","before":"leak","after":"healthy"`) {
		t.Errorf("the detail does not show how MODE changes: %s", body)
	}
}

// The list says of each incident, in brief, what happened and what Themis
// proposes: the cause the investigation leads with, in its pack's words,
// the fix, and why it last changed state.
func TestListIncidents(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE investigations SET hypotheses = $2 WHERE incident_id = $1`, a.IncidentID,
		`[{"rootCauseClass":"memory_limit_regression","status":"HYPOTHESIS_STATUS_REFUTED"},{"rootCauseClass":"new_version_broken","status":"HYPOTHESIS_STATUS_SUPPORTED"}]`); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, http.MethodGet, "/v1/incidents", token, nil)
	var list []struct {
		ID, State string
		Summary   struct {
			Cause struct{ Name, Description string }
			Fix   struct {
				Capability string
				Params     map[string]any
				Words      struct{ Noun, Act string }
			}
			Rule, Reason, From string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != a.IncidentID || list[0].State != "awaiting_approval" {
		t.Fatalf("list = %+v", list)
	}
	sum := list[0].Summary
	if sum.Cause.Name != "new_version_broken" || sum.Cause.Description != "a recent rollout brought a version that fails" {
		t.Errorf("cause = %+v, want the first not ruled out, in its pack's words", sum.Cause)
	}
	if sum.Fix.Capability != "deployment.rollback-to-revision" || sum.Fix.Params["revision"] != float64(1) ||
		sum.Fix.Words.Noun != "rollback" || sum.Fix.Words.Act != "Roll checkout back to revision 1" {
		t.Errorf("fix = %+v", sum.Fix)
	}
	if sum.From != "proposed" || sum.Reason != "test" || sum.Rule != a.PolicyID {
		t.Errorf("last change = %q from %q, decided by %q", sum.Reason, sum.From, sum.Rule)
	}
}

// requestLog returns the debug line of the last request.
func (e *env) requestLog(t *testing.T) map[string]any {
	t.Helper()
	ls := strings.Split(strings.TrimSpace(e.logs.String()), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(ls[len(ls)-1]), &m); err != nil || m["msg"] != "API request served" {
		t.Fatalf("last line is not a request line: %q", ls[len(ls)-1])
	}
	return m
}

func TestApprovalAPI_LogsCarryIncidentID(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)

	e.approve(t, a, "sha256:stale")
	l := e.requestLog(t)
	if l[telemetry.FieldActionID] != a.ID || l[telemetry.FieldIncidentID] != a.IncidentID || l["status"] != float64(http.StatusConflict) {
		t.Errorf("approve: %v, want action_id, incident_id and 409", l)
	}

	e.do(t, http.MethodGet, "/v1/incidents/"+a.IncidentID, token, nil)
	if l := e.requestLog(t); l[telemetry.FieldIncidentID] != a.IncidentID || l["route"] != "GET /v1/incidents/{id}" {
		t.Errorf("detail: %v, want incident_id and the route", l)
	}
}

// The path is the caller's: only IDs are logged, and only once authorized.
func TestApprovalAPI_LogsNoUntrustedPath(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)

	e.do(t, http.MethodGet, "/v1/incidents/not-an-id", token, nil)
	if l := e.requestLog(t); l[telemetry.FieldIncidentID] != nil {
		t.Errorf("malformed id logged: %v", l)
	}
	e.do(t, http.MethodPost, "/v1/actions/"+a.ID+"/approve", "guess", map[string]string{"digest": a.Digest})
	if l := e.requestLog(t); l[telemetry.FieldActionID] != nil || l["status"] != float64(http.StatusUnauthorized) {
		t.Errorf("unauthorized request: %v, want no action_id and 401", l)
	}
}

// The page shows these answers as they are: they must read as advice.
func TestErrors_ArePlainSentences(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	for name, c := range map[string]struct {
		rec  *httptest.ResponseRecorder
		code int
		want string
	}{
		"stale digest": {e.approve(t, a, "sha256:stale"), http.StatusConflict, "This fix changed since you opened it. Reload the page and review it again."},
		"unknown":      {e.do(t, http.MethodPost, "/v1/actions/0192f0c1-0000-7000-8000-0000000000ff/approve", token, map[string]string{"digest": "x"}), http.StatusNotFound, "No such incident or fix."},
	} {
		if got := strings.TrimSpace(c.rec.Body.String()); c.rec.Code != c.code || got != c.want {
			t.Errorf("%s: %d %q, want %d %q", name, c.rec.Code, got, c.code, c.want)
		}
	}
	e.approve(t, a, a.Digest)
	if got := strings.TrimSpace(e.approve(t, a, a.Digest).Body.String()); got != "This fix was already approved, denied or canceled." {
		t.Errorf("approved twice: body = %q", got)
	}
}

func (e *env) detail(t *testing.T, id string) map[string]any {
	t.Helper()
	rec := e.do(t, http.MethodGet, "/v1/incidents/"+id, token, nil)
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("detail: %v: %s", err, rec.Body)
	}
	return d
}

// The page tells the operator what evidence was missing, and how to let
// Themis read it.
func TestIncidentDetail_ShowsWhatWasMissingWithRemedies(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		return e.store.InsertEvidenceGap(t.Context(), tx, store.EvidenceGap{IncidentID: a.IncidentID, Template: evidence.RolloutHistory, Cause: evidence.GapForbidden, At: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	missing, _ := e.detail(t, a.IncidentID)["Missing"].([]any)
	if len(missing) != 1 || !strings.Contains(missing[0].(string), "Add shop to enrolledNamespaces.") {
		t.Errorf("Missing = %v", missing)
	}
}

// An investigation that couldn't decide opens with which kind of stop it was.
func TestIncidentDetail_HeadlineForAStopWithoutConclusion(t *testing.T) {
	e := newEnv(t)
	a := e.awaiting(t)
	if d := e.detail(t, a.IncidentID); d["Headline"] != nil {
		t.Errorf("an investigation that decided has headline %v", d["Headline"])
	}
	if _, err := e.pool.Exec(t.Context(), `UPDATE investigations SET conclusion = 'inconclusive', blind = true WHERE incident_id = $1`, a.IncidentID); err != nil {
		t.Fatal(err)
	}
	// Someone's suggestion moved it on: what's next matters more now.
	if got := e.detail(t, a.IncidentID)["Headline"]; got != nil {
		t.Errorf("an incident awaiting approval has headline %v", got)
	}
	// The investigation's headline holds while it is what ended the incident.
	for _, state := range []incident.State{incident.Inconclusive, incident.ClosedNoAction} {
		if _, err := e.pool.Exec(t.Context(), `UPDATE incidents SET state = $2 WHERE id = $1`, a.IncidentID, state); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(t.Context(), `INSERT INTO transitions (incident_id, from_state, to_state, reason, actor) VALUES ($1, 'investigating', $2, 'No fix proposed.', $3)`, a.IncidentID, state, investigation.Investigator); err != nil {
			t.Fatal(err)
		}
		if got := e.detail(t, a.IncidentID)["Headline"]; got != hint.HeadlineBlind {
			t.Errorf("%s: Headline = %v, want %q", state, got, hint.HeadlineBlind)
		}
	}
	// One closed for another reason says that reason instead.
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO transitions (incident_id, from_state, to_state, reason, actor) VALUES ($1, 'awaiting_suggestion', 'closed_no_action', 'Closed by alice.', 'alice')`, a.IncidentID); err != nil {
		t.Fatal(err)
	}
	if got := e.detail(t, a.IncidentID)["Headline"]; got != nil {
		t.Errorf("closed by a person: Headline = %v, want none", got)
	}
}

// A person may suggest any fix the incident's profiles offer: each once,
// primary profile first, with what it does and the schema of its values. A
// profile no longer configured offers nothing.
func TestIncidentDetail_OffersEveryFixOfItsProfiles(t *testing.T) {
	e := newEnv(t)
	cfg := config.Config{Profiles: []config.Profile{
		{Name: "rollout-regression", Capabilities: []string{rollback.Name}},
		{Name: "oom-limit-too-low", Capabilities: []string{resources.Name, rollback.Name}},
		{Name: "probe-restart-storm", Capabilities: []string{probes.Name}},
	}}
	mux := http.NewServeMux()
	(&httpapi.API{Store: e.store, TokenFile: tokenFile(t), Fixes: catalog.Fixes(), Config: cfg}).Register(mux)
	e.handler = mux
	var id string
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		id, _, err = e.store.CreateOrJoinIncident(t.Context(), tx, store.IncidentArgs{
			Fingerprint: "fp", Profile: "rollout-regression", AlsoMatched: []string{"oom-limit-too-low", "gone", "probe-restart-storm"},
			Target: target, Payload: []byte(`{}`),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, http.MethodGet, "/v1/incidents/"+id, token, nil)
	var d struct {
		Fixes []struct {
			Name, Noun, Description string
			Schema                  struct{ Type string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("detail: %v: %s", err, rec.Body)
	}
	var names []string
	for _, f := range d.Fixes {
		names = append(names, f.Name)
		if f.Noun == "" || f.Description == "" || f.Schema.Type != "object" {
			t.Errorf("%s: %+v, want its noun, description and schema", f.Name, f)
		}
	}
	if want := []string{rollback.Name, resources.Name, probes.Name}; !slices.Equal(names, want) {
		t.Errorf("fixes = %v, want %v", names, want)
	}
}
