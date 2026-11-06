package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/server/httpapi"
)

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
