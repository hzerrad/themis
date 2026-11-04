package identity_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
)

var now = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// provider is an OIDC identity provider that signs in whoever it's told to.
type provider struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu sync.Mutex
	// challenge and nonce are what the last sign-in sent.
	challenge, nonce string
	// claims override or add to the ID token's claims.
	claims map[string]any
	// signer, when set, signs instead of the published key.
	signer *rsa.PrivateKey
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &provider{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.URL, "authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
			"jwks_uri": p.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		defer p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		claims := map[string]any{
			"iss": p.URL, "aud": "themis-ui", "sub": "sub-alice", "nonce": p.nonce,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			"email": "alice@example.com", "email_verified": true, "groups": []string{"sre", "oncall"},
		}
		for k, v := range p.claims {
			claims[k] = v
		}
		signWith := p.key
		if p.signer != nil {
			signWith = p.signer
		}
		sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signWith}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		raw, err := jwt.Signed(sig).Claims(claims).Serialize()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": raw})
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

// memStore keeps sign-ins and sessions in memory.
type memStore struct {
	mu       sync.Mutex
	signIns  map[string]store.SignIn
	sessions map[string]store.Session
}

func newMemStore() *memStore {
	return &memStore{signIns: map[string]store.SignIn{}, sessions: map[string]store.Session{}}
}

func (m *memStore) StartSignIn(_ context.Context, in store.SignIn, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signIns[in.State] = in
	return nil
}

func (m *memStore) FinishSignIn(_ context.Context, state string, at time.Time) (store.SignIn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.signIns[state]
	delete(m.signIns, state)
	if !ok || !in.ExpiresAt.After(at) {
		return store.SignIn{}, store.ErrNotFound
	}
	return in, nil
}

func (m *memStore) CreateSession(_ context.Context, id string, se store.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = se
	return nil
}

func (m *memStore) Session(_ context.Context, id string, at time.Time) (store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	se, ok := m.sessions[id]
	if !ok || !se.ExpiresAt.After(at) {
		return store.Session{}, store.ErrNotFound
	}
	return se, nil
}

func (m *memStore) EndSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

type rig struct {
	idp   *provider
	store *memStore
	mux   *http.ServeMux
}

func newRig(t *testing.T) *rig {
	t.Helper()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &rig{idp: newProvider(t), store: newMemStore(), mux: http.NewServeMux()}
	(&identity.SignIn{
		Config: config.OIDC{IssuerURL: r.idp.URL, ClientID: "themis-ui", ClientSecretFile: secret, RedirectURL: "https://themis.example.com/auth/callback"},
		Store:  r.store, Now: func() time.Time { return now },
	}).Register(r.mux)
	(&identity.SignOut{Store: r.store, Logger: slog.New(slog.DiscardHandler)}).Register(r.mux)
	return r
}

func (r *rig) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

// login starts a sign-in, as the provider would see it, and returns its state
// and the browser's sign-in cookie.
func (r *rig) login(t *testing.T) (string, *http.Cookie) {
	t.Helper()
	rec := r.serve(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body)
	}
	to, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := to.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "themis-ui" || q.Get("scope") != "openid email profile" {
		t.Fatalf("authorization request = %s", to)
	}
	r.idp.mu.Lock()
	r.idp.challenge, r.idp.nonce = q.Get("code_challenge"), q.Get("nonce")
	r.idp.mu.Unlock()
	return q.Get("state"), cookieNamed(t, rec, "themis_sign_in")
}

func (r *rig) callback(t *testing.T, state string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/callback?code=the-code&state="+url.QueryEscape(state), nil)
	if c != nil {
		req.AddCookie(c)
	}
	return r.serve(req)
}

func cookieNamed(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", name, rec.Result().Header["Set-Cookie"])
	return nil
}

// A sign-in starts a session, and can't be finished twice, so a leaked
// callback URL is useless.
func TestSignIn_StartsASession(t *testing.T) {
	r := newRig(t)
	state, c := r.login(t)
	rec := r.callback(t, state, c)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	session := cookieNamed(t, rec, identity.SessionCookie)
	if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie = %+v, want HttpOnly, Secure and SameSite=Lax", session)
	}
	se, err := r.store.Session(t.Context(), session.Value, now)
	if err != nil {
		t.Fatal(err)
	}
	if se.Username != "alice@example.com" || se.Subject != "sub-alice" || !slices.Equal(se.Groups, []string{"sre", "oncall"}) {
		t.Errorf("session = %+v", se)
	}
	if !se.ExpiresAt.Equal(now.Add(config.DefaultSessionTTL)) {
		t.Errorf("session expires at %s, want %s", se.ExpiresAt, now.Add(config.DefaultSessionTTL))
	}
	if rec := r.callback(t, state, c); rec.Code != http.StatusBadRequest {
		t.Errorf("second callback = %d, want 400", rec.Code)
	}
}

func TestSignIn_Refused(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		claims  map[string]any
		signer  *rsa.PrivateKey
		cookie  func(*http.Cookie) *http.Cookie
		state   func(string) string
		want    int
		message string
	}{
		"another browser's sign-in":         {cookie: func(*http.Cookie) *http.Cookie { return nil }, want: http.StatusBadRequest, message: "didn't start in this browser"},
		"a forged state":                    {state: func(string) string { return "forged" }, want: http.StatusBadRequest, message: "didn't start in this browser"},
		"another sign-in's nonce":           {claims: map[string]any{"nonce": "replayed"}, want: http.StatusUnauthorized, message: "didn't confirm who you are"},
		"a token for another app":           {claims: map[string]any{"aud": "someone-else"}, want: http.StatusUnauthorized, message: "didn't confirm who you are"},
		"an expired token":                  {claims: map[string]any{"exp": now.Add(-time.Minute).Unix()}, want: http.StatusUnauthorized, message: "didn't confirm who you are"},
		"a key the provider didn't publish": {signer: other, want: http.StatusUnauthorized, message: "didn't confirm who you are"},
		"an unverified address":             {claims: map[string]any{"email_verified": false}, want: http.StatusUnauthorized, message: "didn't confirm who you are"},
		"no username":                       {claims: map[string]any{"email": ""}, want: http.StatusUnauthorized, message: "didn't confirm who you are"},
		"the admin token's name":            {claims: map[string]any{"email": "admin"}, want: http.StatusUnauthorized, message: "didn't confirm who you are"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			state, c := r.login(t)
			r.idp.claims, r.idp.signer = tc.claims, tc.signer
			if tc.cookie != nil {
				c = tc.cookie(c)
			}
			if tc.state != nil {
				state = tc.state(state)
			}
			rec := r.callback(t, state, c)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.message) {
				t.Errorf("callback = %d %q, want %d containing %q", rec.Code, rec.Body, tc.want, tc.message)
			}
			if len(r.store.sessions) != 0 {
				t.Errorf("a session was started: %v", r.store.sessions)
			}
		})
	}
}

// The provider refusing the code verifier ends the sign-in.
func TestSignIn_NeedsTheCodeVerifier(t *testing.T) {
	r := newRig(t)
	state, c := r.login(t)
	r.idp.mu.Lock()
	r.idp.challenge = "not-this-sign-ins"
	r.idp.mu.Unlock()
	if rec := r.callback(t, state, c); rec.Code != http.StatusUnauthorized {
		t.Errorf("callback = %d, want 401", rec.Code)
	}
}

func TestSignOut(t *testing.T) {
	r := newRig(t)
	state, c := r.login(t)
	session := cookieNamed(t, r.callback(t, state, c), identity.SessionCookie)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil)
	req.AddCookie(session)
	if rec := r.serve(req); rec.Code != http.StatusForbidden {
		t.Errorf("sign-out without the request header = %d, want 403", rec.Code)
	}
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil)
	req.AddCookie(session)
	req.Header.Set(identity.RequestHeader, "1")
	if rec := r.serve(req); rec.Code != http.StatusNoContent {
		t.Fatalf("sign-out = %d: %s", rec.Code, rec.Body)
	}
	if _, err := r.store.Session(t.Context(), session.Value, now); err == nil {
		t.Error("the session outlived sign-out")
	}
}

type sessionEnder struct{ err error }

func (s sessionEnder) EndSession(context.Context, string) error { return s.err }

// Sign-out clears the cookie however it was set. A browser ignores a secure
// Set-Cookie over plain HTTP, where OIDC sets a plain cookie, and over HTTPS
// lets a non-secure deletion replace a secure one.
func TestSignOut_ClearsTheCookieHoweverItWasSet(t *testing.T) {
	for _, addr := range []string{"http://themis.internal", "https://themis.example.com", "http://localhost:8090"} {
		mux := http.NewServeMux()
		(&identity.SignOut{Store: sessionEnder{}}).Register(mux)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, addr+"/auth/logout", nil)
		req.Header.Set(identity.RequestHeader, "1")
		req.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: "s"}) //nolint:gosec // a cookie the browser sends back
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		c := cookieNamed(t, rec, identity.SessionCookie)
		if c.Secure || c.MaxAge >= 0 {
			t.Errorf("%s: sign-out cookie secure=%v max-age=%d, want a non-secure deletion", addr, c.Secure, c.MaxAge)
		}
	}
}

// With no logger set, a failed sign-out logs to the default logger.
func TestSignOut_WithoutALogger(t *testing.T) {
	mux := http.NewServeMux()
	(&identity.SignOut{Store: sessionEnder{err: errors.New("database down")}}).Register(mux)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil)
	req.Header.Set(identity.RequestHeader, "1")
	req.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: "s"}) //nolint:gosec // a cookie the browser sends back
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("sign-out with the database down = %d, want 503", rec.Code)
	}
}
