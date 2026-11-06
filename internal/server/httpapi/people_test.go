package httpapi_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// signedIn is an API where people sign in, with a session for each of
// viewer, responder, approver, admin and nobody (no role).
func signedIn(t *testing.T) *env {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{store: s, pool: pool, clock: now}
	for _, name := range []string{"viewer", "responder", "approver", "admin", "nobody"} {
		se := store.Session{Subject: name, Username: name + "@example.com", Groups: []string{name + "s"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := s.CreateSession(t.Context(), "session-"+name, se); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	log := telemetry.NewLogger(&e.logs, "core", slog.LevelDebug)
	(&httpapi.API{
		Store: s, TokenFile: file, Sessions: s, OIDC: true, Accounts: s, Setup: &identity.Setup{Store: s, Logger: log}, Fixes: catalog.Fixes(), Now: func() time.Time { return e.clock },
		Access: []config.AccessBinding{
			{Role: config.RoleViewer, Groups: []string{"viewers"}},
			{Role: config.RoleResponder, Users: []string{"responder@example.com"}},
			{Role: config.RoleApprover, Groups: []string{"approvers"}},
			{Role: config.RoleAdmin, Users: []string{"admin@example.com"}},
		},
		Logger: log,
	}).Register(mux)
	e.handler = mux
	return e
}

// as sends a request with name's session, and the header the UI adds.
func (e *env) as(t *testing.T, name, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, &b)
	req.AddCookie(&http.Cookie{Name: "themis_session", Value: "session-" + name, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	req.Header.Set("X-Themis-Request", "1")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func TestPeople_RolesDecideWhatTheyMayDo(t *testing.T) {
	e := signedIn(t)
	a := e.awaiting(t)
	approve := "/v1/actions/" + a.ID + "/approve"
	for _, tc := range []struct {
		who, method, path string
		want              int
	}{
		{"viewer", http.MethodGet, "/v1/incidents", http.StatusOK},
		{"viewer", http.MethodPost, approve, http.StatusForbidden},
		{"responder", http.MethodPost, approve, http.StatusForbidden},
		{"nobody", http.MethodGet, "/v1/incidents", http.StatusForbidden},
		{"stranger", http.MethodGet, "/v1/incidents", http.StatusUnauthorized},
	} {
		if rec := e.as(t, tc.who, tc.method, tc.path, map[string]string{"digest": a.Digest}); rec.Code != tc.want {
			t.Errorf("%s %s %s = %d %q, want %d", tc.who, tc.method, tc.path, rec.Code, rec.Body, tc.want)
		}
	}
	if rec := e.as(t, "nobody", http.MethodGet, "/v1/incidents", nil); !strings.Contains(rec.Body.String(), "nobody@example.com") {
		t.Errorf("someone without access isn't told why: %q", rec.Body)
	}
}

// The approval names the person who gave it, not a shared token.
func TestPeople_ApprovalRecordsWhoApproved(t *testing.T) {
	e := signedIn(t)
	a := e.awaiting(t)
	if rec := e.as(t, "approver", http.MethodPost, "/v1/actions/"+a.ID+"/approve", map[string]string{"digest": a.Digest}); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body)
	}
	var approver string
	if err := e.pool.QueryRow(t.Context(), `SELECT approver FROM approvals WHERE action_id = $1`, a.ID).Scan(&approver); err != nil {
		t.Fatal(err)
	}
	if approver != "approver@example.com" {
		t.Errorf("approver = %q, want approver@example.com", approver)
	}
	transitions, err := e.store.Transitions(t.Context(), a.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	last := transitions[len(transitions)-1]
	if last.Actor != "approver@example.com" || last.Reason != "Approved by approver@example.com." {
		t.Errorf("last transition = %+v", last)
	}
}

// A form on another site can send the cookie, but not the header.
func TestPeople_ChangesNeedTheRequestHeader(t *testing.T) {
	e := signedIn(t)
	a := e.awaiting(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/actions/"+a.ID+"/approve", strings.NewReader(`{"digest":"`+a.Digest+`"}`))
	req.AddCookie(&http.Cookie{Name: "themis_session", Value: "session-approver", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("approve without the header = %d, want 403", rec.Code)
	}
	if got := e.state(t, a.IncidentID); got != "awaiting_approval" {
		t.Errorf("state = %s, want awaiting_approval", got)
	}
}

// Raw evidence, such as logs, is for responders and above, who get its data
// as JSON; a viewer is only told it exists. Everyone is warned when some looks
// like instructions, but only responders read what it says.
func TestPeople_RawEvidenceIsForResponders(t *testing.T) {
	e := signedIn(t)
	a := e.awaiting(t)
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		return e.store.InsertEvidence(t.Context(), tx, a.IncidentID, "", nil, evidence.Item{
			ID: "0192f0c1-0000-7000-8000-00000000000b", Template: evidence.ContainerLogsTail, Data: []byte(`{"logs":"canary-line"}`),
			CollectedAt: now, Classification: evidence.ClassificationSensitive,
			Suspicious: []evidence.Suspicion{{Reason: evidence.ReasonInstructions, At: "/logs", Excerpt: "canary-line"}},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for who, sees := range map[string]bool{"viewer": false, "responder": true, "approver": true} {
		rec := e.as(t, who, http.MethodGet, "/v1/incidents/"+a.IncidentID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: detail = %d", who, rec.Code)
		}
		if got := strings.Contains(rec.Body.String(), "canary-line"); got != sees {
			t.Errorf("%s sees the logs: %v, want %v", who, got, sees)
		}
		if !strings.Contains(rec.Body.String(), evidence.ContainerLogsTail) {
			t.Errorf("%s isn't told the evidence exists", who)
		}
		var warned struct{ EvidenceWarning string }
		if err := json.Unmarshal(rec.Body.Bytes(), &warned); err != nil || !strings.HasPrefix(warned.EvidenceWarning, "Some evidence contains text addressed to Themis") {
			t.Errorf("%s: warning = %q (%v)", who, warned.EvidenceWarning, err)
		}
		if sees {
			var detail struct {
				Evidence []struct{ Data map[string]any }
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || len(detail.Evidence) != 1 || detail.Evidence[0].Data["logs"] != "canary-line" {
				t.Errorf("%s: evidence = %s (%v), want its data as JSON", who, rec.Body, err)
			}
		}
	}
}

func TestPeople_Me(t *testing.T) {
	e := signedIn(t)
	var me struct {
		Username, Role string
		Token          bool
	}
	rec := e.as(t, "approver", http.MethodGet, "/v1/me", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("me = %d %q: %v", rec.Code, rec.Body, err)
	}
	if me.Username != "approver@example.com" || me.Role != config.RoleApprover || me.Token {
		t.Errorf("me = %+v", me)
	}
	// Someone without access may still learn who they're signed in as.
	if rec := e.as(t, "nobody", http.MethodGet, "/v1/me", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"role":""`) {
		t.Errorf("me for nobody = %d %q", rec.Code, rec.Body)
	}
	rec = e.do(t, http.MethodGet, "/v1/me", token, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || me.Role != config.RoleAdmin || !me.Token {
		t.Errorf("me with the token = %+v, %v", me, err)
	}
}

// Setup finishes, retiring the initial admin password, when an admin signs in
// through the identity provider or with an account: not with the token, and
// not as anyone less trusted.
func TestPeople_AnAdminSigningInFinishesSetup(t *testing.T) {
	e := signedIn(t)
	finished := func() bool {
		t.Helper()
		done, err := e.store.SetupFinished(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return done
	}
	e.as(t, "approver", http.MethodGet, "/v1/me", nil)
	e.do(t, http.MethodGet, "/v1/me", token, nil)
	if finished() {
		t.Fatal("setup finished without an admin signing in")
	}
	if rec := e.as(t, "admin", http.MethodGet, "/v1/me", nil); rec.Code != http.StatusOK {
		t.Fatalf("me for admin = %d %q", rec.Code, rec.Body)
	}
	if !finished() {
		t.Error("setup still open after an admin signed in")
	}
	if logs := e.logs.String(); !strings.Contains(logs, `"msg":"initial admin password retired"`) || !strings.Contains(logs, `"by":"admin@example.com"`) {
		t.Errorf("no log says admin@example.com retired the initial admin password: %s", e.logs.String())
	}
}

func TestPeople_SignInMethods(t *testing.T) {
	for name, tc := range map[string]struct {
		e    func(*testing.T) *env
		want string
	}{
		"with OIDC":    {signedIn, `"oidc":true`},
		"without OIDC": {newEnv, `"oidc":false`},
	} {
		rec := tc.e(t).do(t, http.MethodGet, "/v1/sign-in", "", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: sign-in methods = %d %q", name, rec.Code, rec.Body)
		}
	}
}
