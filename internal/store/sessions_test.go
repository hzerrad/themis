package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/store"
)

func TestSignIn_IsUsedOnce(t *testing.T) {
	s, _ := newStore(t)
	in := store.SignIn{State: "st", Nonce: "n", Verifier: "v", ExpiresAt: t0.Add(5 * time.Minute)}
	if err := s.StartSignIn(t.Context(), in, t0); err != nil {
		t.Fatal(err)
	}
	got, err := s.FinishSignIn(t.Context(), "st", t0.Add(time.Minute))
	if err != nil || got.Nonce != "n" || got.Verifier != "v" {
		t.Fatalf("FinishSignIn = %+v, %v", got, err)
	}
	if _, err := s.FinishSignIn(t.Context(), "st", t0.Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second FinishSignIn: %v, want ErrNotFound", err)
	}
}

func TestSignIn_Expires(t *testing.T) {
	s, _ := newStore(t)
	in := store.SignIn{State: "st", Nonce: "n", Verifier: "v", ExpiresAt: t0.Add(5 * time.Minute)}
	if err := s.StartSignIn(t.Context(), in, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishSignIn(t.Context(), "st", t0.Add(5*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FinishSignIn after expiry: %v, want ErrNotFound", err)
	}
}

func TestSession_RoundTripAndEnd(t *testing.T) {
	s, pool := newStore(t)
	se := store.Session{Subject: "sub-1", Username: "alice@example.com", Groups: []string{"sre"}, CreatedAt: t0, ExpiresAt: t0.Add(8 * time.Hour)}
	if err := s.CreateSession(t.Context(), "secret-id", se); err != nil {
		t.Fatal(err)
	}
	got, err := s.Session(t.Context(), "secret-id", t0.Add(time.Hour))
	if err != nil || got.Username != "alice@example.com" || !slices.Equal(got.Groups, []string{"sre"}) {
		t.Fatalf("Session = %+v, %v", got, err)
	}
	var stored int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE id_hash = convert_to('secret-id', 'UTF8')`).Scan(&stored); err != nil || stored != 0 {
		t.Errorf("the session ID itself is stored (%d rows, %v)", stored, err)
	}
	if _, err := s.Session(t.Context(), "secret-id", t0.Add(8*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired Session: %v, want ErrNotFound", err)
	}
	if err := s.EndSession(t.Context(), "secret-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(t.Context(), "secret-id", t0.Add(time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Session after EndSession: %v, want ErrNotFound", err)
	}
}
