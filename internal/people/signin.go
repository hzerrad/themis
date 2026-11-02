package people

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/hzerrad/themis/internal/auth"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
)

// signInCookie ties a sign-in to the browser that started it, so no one can
// sign someone else in with their own account.
const signInCookie = "themis_sign_in"

// signInTTL is how long someone has to finish signing in.
const signInTTL = 10 * time.Minute

// SignInStore keeps sign-ins under way and the sessions they start.
type SignInStore interface {
	Sessions
	StartSignIn(ctx context.Context, in store.SignIn, now time.Time) error
	FinishSignIn(ctx context.Context, state string, now time.Time) (store.SignIn, error)
	CreateSession(ctx context.Context, id string, se store.Session) error
	EndSession(ctx context.Context, id string) error
}

// SignIn signs people in with an OIDC provider, using the authorization code
// flow with PKCE.
type SignIn struct {
	Config config.OIDC
	Store  SignInStore
	Logger *slog.Logger
	Now    func() time.Time
	// Client is used to reach the provider; nil means http.DefaultClient.
	Client *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
}

// Register serves the sign-in routes on mux.
func (s *SignIn) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", s.login)
	mux.HandleFunc("GET /auth/callback", s.callback)
	mux.HandleFunc("POST /auth/logout", s.logout)
}

// discover finds the provider's endpoints and keys, and keeps them once found,
// so Themis starts even when the provider is down.
func (s *SignIn) discover(ctx context.Context) (*oidc.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider != nil {
		return s.provider, nil
	}
	p, err := oidc.NewProvider(s.clientContext(ctx), s.Config.IssuerURL)
	if err != nil {
		return nil, err
	}
	s.provider = p
	return p, nil
}

func (s *SignIn) clientContext(ctx context.Context) context.Context {
	if s.Client == nil {
		return ctx
	}
	return oidc.ClientContext(ctx, s.Client)
}

func (s *SignIn) oauth(p *oidc.Provider, secret string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: s.Config.ClientID, ClientSecret: secret, RedirectURL: s.Config.RedirectURL,
		Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email", "profile", "groups"},
	}
}

func (s *SignIn) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.discover(ctx)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "Themis can't reach the identity provider. Try again shortly.", err)
		return
	}
	in := store.SignIn{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), ExpiresAt: s.now().Add(signInTTL)}
	if err := s.Store.StartSignIn(ctx, in, s.now()); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Themis can't sign you in right now. Try again shortly.", err)
		return
	}
	http.SetCookie(w, s.cookie(signInCookie, in.State, "/auth", signInTTL))
	// The secret isn't needed to start: the browser carries no secret.
	to := s.oauth(p, "").AuthCodeURL(in.State, oidc.Nonce(in.Nonce), oauth2.S256ChallengeOption(in.Verifier))
	http.Redirect(w, r, to, http.StatusFound)
}

func (s *SignIn) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.fail(w, r, http.StatusUnauthorized, "The identity provider didn't sign you in. Try again.", errors.New(e+": "+q.Get("error_description")))
		return
	}
	c, err := r.Cookie(signInCookie)
	if err != nil || c.Value == "" || c.Value != q.Get("state") {
		s.fail(w, r, http.StatusBadRequest, "This sign-in didn't start in this browser. Sign in again.", errors.New("state does not match the sign-in cookie"))
		return
	}
	http.SetCookie(w, s.cookie(signInCookie, "", "/auth", -1))
	in, err := s.Store.FinishSignIn(ctx, c.Value, s.now())
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, http.StatusBadRequest, "This sign-in expired or was already used. Sign in again.", err)
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Themis can't sign you in right now. Try again shortly.", err)
		return
	}
	se, err := s.exchange(ctx, q.Get("code"), in)
	if err != nil {
		s.fail(w, r, http.StatusUnauthorized, "The identity provider didn't confirm who you are. Sign in again.", err)
		return
	}
	id := random()
	if err := s.Store.CreateSession(ctx, id, se); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Themis can't sign you in right now. Try again shortly.", err)
		return
	}
	s.logger().InfoContext(ctx, "a person signed in", "username", se.Username)
	http.SetCookie(w, s.cookie(SessionCookie, id, "/", se.ExpiresAt.Sub(se.CreatedAt)))
	http.Redirect(w, r, "/", http.StatusFound)
}

// exchange trades code for an ID token, checks it, and says who it names.
func (s *SignIn) exchange(ctx context.Context, code string, in store.SignIn) (store.Session, error) {
	p, err := s.discover(ctx)
	if err != nil {
		return store.Session{}, err
	}
	secret, err := auth.ReadToken(s.Config.ClientSecretFile)
	if err != nil {
		return store.Session{}, fmt.Errorf("read the client secret: %w", err)
	}
	cctx := s.clientContext(ctx)
	tok, err := s.oauth(p, string(secret)).Exchange(cctx, code, oauth2.VerifierOption(in.Verifier))
	if err != nil {
		return store.Session{}, fmt.Errorf("exchange the code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return store.Session{}, errors.New("the token response has no ID token")
	}
	idt, err := p.VerifierContext(cctx, &oidc.Config{ClientID: s.Config.ClientID, Now: s.now}).Verify(cctx, raw)
	if err != nil {
		return store.Session{}, fmt.Errorf("verify the ID token: %w", err)
	}
	if idt.Nonce != in.Nonce {
		return store.Session{}, errors.New("the ID token's nonce doesn't match the sign-in")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return store.Session{}, fmt.Errorf("read the ID token's claims: %w", err)
	}
	cfg := s.Config.WithDefaults()
	username, _ := claims[cfg.UsernameClaim].(string)
	if strings.TrimSpace(username) == "" {
		return store.Session{}, fmt.Errorf("the ID token has no %s claim", cfg.UsernameClaim)
	}
	// An address the provider hasn't verified could be anyone's.
	if verified, ok := claims["email_verified"].(bool); cfg.UsernameClaim == "email" && ok && !verified {
		return store.Session{}, errors.New("the email address isn't verified")
	}
	var groups []string
	if gs, ok := claims[cfg.GroupsClaim].([]any); ok {
		for _, g := range gs {
			if g, ok := g.(string); ok {
				groups = append(groups, g)
			}
		}
	}
	now := s.now()
	return store.Session{Subject: idt.Subject, Username: username, Groups: groups, CreatedAt: now, ExpiresAt: now.Add(cfg.SessionTTL.Duration)}, nil
}

func (s *SignIn) logout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(RequestHeader) == "" {
		http.Error(w, "Sign out from the Themis page.", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		if err := s.Store.EndSession(r.Context(), c.Value); err != nil {
			s.fail(w, r, http.StatusServiceUnavailable, "Themis can't sign you out right now. Try again shortly.", err)
			return
		}
	}
	http.SetCookie(w, s.cookie(SessionCookie, "", "/", -1))
	w.WriteHeader(http.StatusNoContent)
}

// cookie is HTTP-only, and secure whenever Themis is reached over HTTPS.
func (s *SignIn) cookie(name, value, path string, ttl time.Duration) *http.Cookie {
	u, _ := url.Parse(s.Config.RedirectURL)
	// Plain HTTP is for trying Themis locally, where a secure cookie would
	// never be sent back.
	c := &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: u != nil && u.Scheme == "https"} //nolint:gosec // Secure whenever the redirect URL is HTTPS
	if ttl < 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(ttl.Seconds())
	}
	return c
}

func (s *SignIn) fail(w http.ResponseWriter, r *http.Request, code int, message string, err error) {
	s.logger().WarnContext(r.Context(), "a sign-in failed", telemetry.FieldError, err)
	http.Error(w, message, code)
}

func (s *SignIn) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SignIn) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// random is 32 random bytes, URL-safe.
func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
