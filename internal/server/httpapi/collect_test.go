package httpapi_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

type fakeCollector struct {
	person, template string
	params           map[string]string
	err              error
}

func (f *fakeCollector) Collect(_ context.Context, _, person, template string, params map[string]string) (evidence.Item, int, error) {
	f.person, f.template, f.params = person, template, params
	if f.err != nil {
		return evidence.Item{}, 0, f.err
	}
	return evidence.Item{Template: template}, 7, nil
}

type fakePreviews struct {
	from suggestion.Suggester
	got  suggestion.Suggestion
	pv   suggestion.Preview
	err  error
}

func (f *fakePreviews) Preview(_ context.Context, from suggestion.Suggester, s suggestion.Suggestion) (suggestion.Preview, error) {
	f.from, f.got = from, s
	return f.pv, f.err
}

// asPeople is a mux serving api, and requests from people signed in with
// the role each needs.
func asPeople(t *testing.T, api *httpapi.API) (*http.ServeMux, func(method, path, who, body string) *httptest.ResponseRecorder) {
	t.Helper()
	s, _ := pgtest.NewStore(t)
	for _, who := range []string{"viewer", "approver"} {
		se := store.Session{Subject: who, Username: who + "@example.com", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := s.CreateSession(t.Context(), "session-"+who, se); err != nil {
			t.Fatal(err)
		}
	}
	api.Store, api.Sessions, api.OIDC, api.Now = s, s, true, func() time.Time { return now }
	api.Access = []config.AccessBinding{
		{Role: config.RoleViewer, Users: []string{"viewer@example.com"}},
		{Role: config.RoleApprover, Users: []string{"approver@example.com"}},
	}
	mux := http.NewServeMux()
	api.Register(mux)
	return mux, func(method, path, who, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "themis_session", Value: "session-" + who, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		req.Header.Set("X-Themis-Request", "1")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
}

// An approver has Themis collect a piece of evidence, and learns its number;
// a viewer may not. A refusal says why, in HTTP and in words.
func TestCollect(t *testing.T) {
	const path = "/v1/incidents/01890000-0000-7000-8000-000000000000/evidence"
	const body = `{"template":"container_logs_tail","params":{"pod":"checkout-x","previous":"true"}}`
	for _, tc := range []struct {
		name, who, body string
		err             error
		want            int
		says            string
	}{
		{name: "an approver", who: "approver", body: body, want: http.StatusOK},
		{name: "a viewer", who: "viewer", body: body, want: http.StatusForbidden, says: "This needs the approver role. You have the viewer role."},
		{name: "not JSON", who: "approver", body: "nope", want: http.StatusBadRequest},
		{name: "busy", who: "approver", body: body, err: &investigation.AskError{Kind: investigation.AskBusy, Message: "Another collection is running."}, want: http.StatusConflict, says: "Another collection is running."},
		{name: "not allowed", who: "approver", body: body, err: &investigation.AskError{Kind: investigation.AskNotAllowed, Message: "Refused."}, want: http.StatusForbidden},
		{name: "invalid", who: "approver", body: body, err: &investigation.AskError{Kind: investigation.AskInvalid, Message: "Refused."}, want: http.StatusBadRequest},
		{name: "too many", who: "approver", body: body, err: &investigation.AskError{Kind: investigation.AskTooMany, Message: "Refused."}, want: http.StatusTooManyRequests},
		{name: "failed", who: "approver", body: body, err: &investigation.AskError{Kind: investigation.AskFailed, Message: "Container logs: access denied in shop."}, want: http.StatusBadGateway},
		{name: "no such incident", who: "approver", body: body, err: store.ErrNotFound, want: http.StatusNotFound},
		{name: "unexpected", who: "approver", body: body, err: errors.New("database gone"), want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCollector{err: tc.err}
			_, do := asPeople(t, &httpapi.API{Collect: f})
			rec := do(http.MethodPost, path, tc.who, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.says != "" && strings.TrimSpace(rec.Body.String()) != tc.says {
				t.Errorf("body = %q, want %q", rec.Body, tc.says)
			}
			if tc.want != http.StatusOK {
				return
			}
			var got struct {
				Number int    `json:"number"`
				Kind   string `json:"kind"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Number != 7 || got.Kind != "Container logs" {
				t.Errorf("answer = %+v (%v)", got, err)
			}
			if f.person != "approver@example.com" || f.template != evidence.ContainerLogsTail || f.params["pod"] != "checkout-x" || f.params["previous"] != "true" {
				t.Errorf("asked %q for %q %v", f.person, f.template, f.params)
			}
		})
	}
}

// Without a collector or a preview, the routes aren't served.
func TestCollectAndPreview_Unserved(t *testing.T) {
	_, do := asPeople(t, &httpapi.API{})
	for _, path := range []string{"/v1/incidents/x/evidence", "/v1/incidents/x/proposals/preview"} {
		if rec := do(http.MethodPost, path, "approver", "{}"); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d", path, rec.Code)
		}
	}
}

// A preview passes the suggestion on as a suggestion is, from the person
// signed in, for approvers only, and refuses in HTTP as a suggestion does.
func TestPreview(t *testing.T) {
	const path = "/v1/incidents/01890000-0000-7000-8000-000000000000/proposals/preview"
	const body = `{"clientProposalId":"ui-1","target":{"kind":"Deployment"},"rollbackToRevision":{"revision":"1"}}`
	f := &fakePreviews{pv: suggestion.Preview{Refused: "Refused: the rollout-regression profile doesn't allow deployment.rollback-to-revision."}}
	_, do := asPeople(t, &httpapi.API{Previews: f})
	if rec := do(http.MethodPost, path, "viewer", body); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer = %d", rec.Code)
	}
	rec := do(http.MethodPost, path, "approver", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Accepted        bool
		Verdict, Reason string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got, json.MatchCaseInsensitiveNames(true)); err != nil {
		t.Fatal(err)
	}
	if got.Accepted || got.Verdict != "Themis would refuse this change." || got.Reason != "The rollout-regression profile doesn't allow deployment.rollback-to-revision." {
		t.Errorf("preview = %+v", got)
	}
	if f.from.Source != "person:approver@example.com" || f.got.Incident != "01890000-0000-7000-8000-000000000000" || f.got.ClientID != "ui-1" {
		t.Errorf("from %q, suggestion %+v", f.from.Source, f.got)
	}

	f.err = &suggestion.Error{Kind: suggestion.Invalid, Message: "target is required"}
	if rec := do(http.MethodPost, path, "approver", body); rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != "target is required" {
		t.Errorf("invalid = %d %q", rec.Code, rec.Body)
	}
}
