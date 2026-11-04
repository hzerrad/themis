package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

type fakeSuggestions struct {
	from   suggestion.Suggester
	got    *suggestion.Suggestion
	stored store.Proposal
	err    error
	// cancel cancels the request before answering.
	cancel context.CancelFunc
}

func (f *fakeSuggestions) Submit(_ context.Context, from suggestion.Suggester, s suggestion.Suggestion) (store.Proposal, error) {
	f.from, f.got = from, &s
	if f.cancel != nil {
		f.cancel()
	}
	return f.stored, f.err
}

// The suggest form says who is suggesting, from the token or the session,
// passes the request on as it came, and answers in HTTP.
func TestSuggest(t *testing.T) {
	const incidentID = "01890000-0000-7000-8000-000000000000"
	body := `{"clientProposalId":"c1","target":{"kind":"Deployment"},"rollbackToRevision":{"revision":"1"},"rationale":"why"}`
	s, _ := pgtest.NewStore(t)
	se := store.Session{Subject: "alex", Username: "alex@example.com", Groups: []string{"approvers"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateSession(t.Context(), "session-alex", se); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	accepted := store.Proposal{ID: "p1", Status: store.ProposalAccepted}
	rejected := store.Proposal{ID: "p2", Status: store.ProposalRejected, RejectionReason: "REJECTION_REASON_TARGET_MISMATCH", Message: "It names another target."}
	for _, tc := range []struct {
		name     string
		signedIn bool
		f        *fakeSuggestions
		body     string
		want     int
		says     string                           // the body, for an error
		resp     *themisv1.SubmitProposalResponse // the body, for an answer
		from     string
	}{
		{
			name: "with the token", f: &fakeSuggestions{stored: accepted}, body: body, want: http.StatusOK,
			resp: &themisv1.SubmitProposalResponse{ProposalId: "p1", Status: themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED}, from: suggestion.PersonSource,
		},
		{
			name: "signed in", signedIn: true, f: &fakeSuggestions{stored: accepted}, body: body, want: http.StatusOK,
			resp: &themisv1.SubmitProposalResponse{ProposalId: "p1", Status: themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED}, from: "person:alex@example.com",
		},
		{
			name: "rejected", f: &fakeSuggestions{stored: rejected}, body: body, want: http.StatusOK,
			resp: &themisv1.SubmitProposalResponse{
				ProposalId: "p2", Status: themisv1.ProposalStatus_PROPOSAL_STATUS_REJECTED,
				RejectionReason: themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH, Message: "It names another target.",
			}, from: suggestion.PersonSource,
		},
		{name: "not JSON", f: &fakeSuggestions{}, body: "nope", want: http.StatusBadRequest},
		{name: "too large", f: &fakeSuggestions{}, body: strings.Repeat(" ", 64<<10+1), want: http.StatusBadRequest},
		{name: "people may not suggest", body: body, want: http.StatusNotFound},
		{name: "invalid", f: &fakeSuggestions{err: &suggestion.Error{Kind: suggestion.Invalid, Message: "target is required"}}, body: body, want: http.StatusBadRequest, says: "target is required"},
		{name: "a lease", f: &fakeSuggestions{err: &suggestion.Error{Kind: suggestion.Forbidden, Message: "a lease belongs to the investigator"}}, body: body, want: http.StatusForbidden, says: "a lease belongs to the investigator"},
		{name: "no such incident", f: &fakeSuggestions{err: &suggestion.Error{Kind: suggestion.NotFound, Message: "no such incident"}}, body: body, want: http.StatusNotFound, says: "no such incident"},
		{name: "too many", f: &fakeSuggestions{err: &suggestion.Error{Kind: suggestion.TooMany, Message: "wait"}}, body: body, want: http.StatusTooManyRequests, says: "wait"},
		{name: "unexpected", f: &fakeSuggestions{err: errors.New("database gone")}, body: body, want: http.StatusInternalServerError, says: "internal error"},
		{name: "canceled", f: &fakeSuggestions{err: errors.New("database gone")}, body: body, want: http.StatusServiceUnavailable, says: context.Canceled.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &httpapi.API{
				Store: s, TokenFile: tokenFile, Sessions: s, OIDC: true, Now: func() time.Time { return now },
				Access: []config.AccessBinding{{Role: config.RoleApprover, Groups: []string{"approvers"}}},
			}
			if tc.f != nil {
				api.Suggestions = tc.f
			}
			mux := http.NewServeMux()
			api.Register(mux)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.name == "canceled" {
				tc.f.cancel = cancel
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/incidents/"+incidentID+"/proposals", strings.NewReader(tc.body))
			if tc.signedIn {
				req.AddCookie(&http.Cookie{Name: "themis_session", Value: "session-alex", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
				req.Header.Set("X-Themis-Request", "1")
			} else {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.says != "" && strings.TrimSpace(rec.Body.String()) != tc.says {
				t.Errorf("body = %q, want %q", rec.Body, tc.says)
			}
			if tc.resp == nil {
				return
			}
			var got themisv1.SubmitProposalResponse
			if err := protojson.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.String() != tc.resp.String() {
				t.Errorf("response = %v (%v), want %v", &got, err, tc.resp)
			}
			s := tc.f.got
			if tc.f.from.Source != tc.from || s.Incident != incidentID || s.ClientID != "c1" || s.Fix != rollback.Name || s.Params["revision"] != int64(1) || s.Rationale != "why" {
				t.Errorf("from %q, suggestion %+v", tc.f.from.Source, s)
			}
		})
	}
}

func TestSuggest_NeedsTheToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f := &fakeSuggestions{}
	(&httpapi.API{TokenFile: tokenFile, Suggestions: f}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/incidents/x/proposals", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized || f.got != nil {
		t.Errorf("status = %d, got = %v", rec.Code, f.got)
	}
}
