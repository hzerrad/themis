package httpapi_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/store"
)

// fakeRenewals says whether a fix can be asked for again, and asks for it
// as by, answering err.
type fakeRenewals struct {
	renewable bool
	by        string
	err       error
}

func (f *fakeRenewals) Renewable(store.Incident, []store.Action) bool { return f.renewable }

func (f *fakeRenewals) Renew(_ context.Context, actionID, by string) (store.Action, error) {
	f.by = by
	if f.err != nil {
		return store.Action{}, f.err
	}
	return store.Action{ID: "0192f0c1-0000-7000-8000-00000000000b", Capability: "deployment.rollback-to-revision", State: store.ActionAwaitingApproval}, nil
}

// An approver asks again for approval of a fix whose approval expired, as
// themselves; a viewer may not. A refusal says why, in HTTP and in the words
// of the refusal.
func TestRenew(t *testing.T) {
	const path = "/v1/actions/01890000-0000-7000-8000-000000000000/renew"
	const changed = "Refused. checkout changed since the fix was planned. Investigate again."
	for _, tc := range []struct {
		name, who string
		err       error
		want      int
		says      string
	}{
		{name: "an approver", who: "approver", want: http.StatusOK},
		{name: "a viewer", who: "viewer", want: http.StatusForbidden, says: "This needs the approver role. You have the viewer role."},
		{name: "refused", who: "approver", err: &governance.Refusal{Reason: changed}, want: http.StatusConflict, says: changed},
		{name: "no such fix", who: "approver", err: store.ErrNotFound, want: http.StatusNotFound, says: "No such incident or fix."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRenewals{err: tc.err}
			_, do := asPeople(t, &httpapi.API{Renewals: f})
			rec := do(http.MethodPost, path, tc.who, "")
			if rec.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.says != "" && strings.TrimSpace(rec.Body.String()) != tc.says {
				t.Errorf("body = %q, want %q", rec.Body, tc.says)
			}
			if tc.want != http.StatusOK {
				return
			}
			var got struct{ ID, State string }
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.State != string(store.ActionAwaitingApproval) || f.by != "approver@example.com" {
				t.Errorf("answer = %+v (%v), asked by %q", got, err, f.by)
			}
		})
	}
}

// The page offers to ask again on the incident's latest fix only while it
// can be.
func TestRenew_Offered(t *testing.T) {
	for _, renewable := range []bool{true, false} {
		e := newEnvWith(t, func(a *httpapi.API) { a.Renewals = &fakeRenewals{renewable: renewable} })
		a := e.awaiting(t)
		actions, _ := e.detail(t, a.IncidentID)["Actions"].([]any)
		if len(actions) != 1 {
			t.Fatalf("actions = %v", actions)
		}
		if got, _ := actions[0].(map[string]any)["Renewable"].(bool); got != renewable {
			t.Errorf("Renewable = %v, want %v", got, renewable)
		}
	}
}

// againEnv serves investigating again, with an investigator installed, and
// requires another approver when another is set.
func againEnv(t *testing.T, another bool) *env {
	t.Helper()
	cfg := config.Config{
		Profiles:  []config.Profile{{Name: "rollout-regression"}},
		Approvals: config.Approvals{RequireAnotherApprover: another},
	}
	return newEnvWith(t, func(a *httpapi.API) {
		a.Config = cfg
		a.Again = &investigation.Again{Store: a.Store, Config: cfg}
	})
}

func (e *env) deny(t *testing.T, id string, body map[string]any) int {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/actions/"+id+"/deny", token, body).Code
}

// A denial keeps the incident open, and offers to investigate it again;
// denying and closing ends it. Investigating again starts once.
func TestDeny_KeepsTheIncidentOpen(t *testing.T) {
	e := againEnv(t, false)
	a := e.awaiting(t)
	if d := e.detail(t, a.IncidentID); d["InvestigateAgain"] != nil {
		t.Errorf("offered to investigate again while a fix awaits approval: %v", d["InvestigateAgain"])
	}
	if code := e.deny(t, a.ID, map[string]any{"reason": "not during the sale"}); code != http.StatusOK {
		t.Fatalf("deny: status = %d", code)
	}
	if got := e.state(t, a.IncidentID); got != incident.AwaitingSuggestion {
		t.Errorf("state = %s, want awaiting_suggestion", got)
	}
	if d := e.detail(t, a.IncidentID); d["InvestigateAgain"] == nil {
		t.Error("not offered to investigate again after a denial")
	}
	path := "/v1/incidents/" + a.IncidentID + "/investigate"
	if rec := e.do(t, http.MethodPost, path, token, nil); rec.Code != http.StatusOK {
		t.Fatalf("investigate again: status = %d: %s", rec.Code, rec.Body)
	}
	if got := e.state(t, a.IncidentID); got != incident.Investigating {
		t.Errorf("state = %s, want investigating", got)
	}
	rec := e.do(t, http.MethodPost, path, token, nil)
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusConflict || got != "Refused. The incident isn't waiting for a fix." {
		t.Errorf("investigating twice: %d %q", rec.Code, got)
	}

	closed := againEnv(t, false)
	b := closed.awaiting(t)
	if code := closed.deny(t, b.ID, map[string]any{"reason": "not during the sale", "close": true}); code != http.StatusOK {
		t.Fatalf("deny and close: status = %d", code)
	}
	if got := closed.state(t, b.IncidentID); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
}

// With another approver required, the person who suggested a fix is refused
// its approval, in a plain sentence, and the page knows it.
func TestApprove_AnotherApprover(t *testing.T) {
	e := againEnv(t, true)
	a := e.awaiting(t)
	// The admin token approves as admin.
	if _, err := e.pool.Exec(t.Context(), `UPDATE proposals SET lease_id = NULL, provenance = '{"source": "person:admin"}' WHERE id = $1`, a.ProposalID); err != nil {
		t.Fatal(err)
	}
	if d := e.detail(t, a.IncidentID); d["AnotherApprover"] != true {
		t.Errorf("AnotherApprover = %v, want true", d["AnotherApprover"])
	}
	rec := e.approve(t, a, a.Digest)
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusForbidden || got != "Refused. A fix needs an approver other than the person who suggested it." {
		t.Errorf("approving one's own suggestion: %d %q", rec.Code, got)
	}
	if got := e.state(t, a.IncidentID); got != incident.AwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval", got)
	}
}
