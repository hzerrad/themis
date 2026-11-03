package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/store"
)

func newAccount(t *testing.T, s *store.Store, username string) {
	t.Helper()
	if err := s.CreateAccount(t.Context(), store.Account{Username: username, PasswordHash: "hash-1", Role: "approver", CreatedAt: t0, CreatedBy: "setup"}); err != nil {
		t.Fatal(err)
	}
}

func TestAccount_CreateAndRead(t *testing.T) {
	s, _ := newStore(t)
	if has, err := s.HasAccounts(t.Context()); err != nil || has {
		t.Fatalf("HasAccounts on a new install = %v, %v", has, err)
	}
	newAccount(t, s, "alice")
	if err := s.CreateAccount(t.Context(), store.Account{Username: "alice", PasswordHash: "x", Role: "viewer", CreatedAt: t0, CreatedBy: "setup"}); !errors.Is(err, store.ErrAccountExists) {
		t.Errorf("second CreateAccount: %v, want ErrAccountExists", err)
	}
	a, err := s.Account(t.Context(), "alice")
	if err != nil || a.Role != "approver" || a.PasswordHash != "hash-1" || a.CreatedBy != "setup" || !a.LockedUntil.Before(t0) {
		t.Fatalf("Account = %+v, %v", a, err)
	}
	if _, err := s.Account(t.Context(), "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown Account: %v, want ErrNotFound", err)
	}
	if all, err := s.Accounts(t.Context()); err != nil || len(all) != 1 {
		t.Errorf("Accounts = %v, %v", all, err)
	}
}

func TestAccount_LocksAfterWrongPasswords(t *testing.T) {
	s, _ := newStore(t)
	newAccount(t, s, "alice")
	for range 2 {
		if err := s.PasswordFailed(t.Context(), "alice", t0, 3, 15*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := s.Account(t.Context(), "alice")
	if a.FailedAttempts != 2 || a.LockedUntil.After(t0) {
		t.Fatalf("after 2 wrong passwords: %+v", a)
	}
	if err := s.PasswordFailed(t.Context(), "alice", t0, 3, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	a, _ = s.Account(t.Context(), "alice")
	if !a.LockedUntil.Equal(t0.Add(15*time.Minute)) || a.FailedAttempts != 0 {
		t.Fatalf("after 3 wrong passwords: %+v, want locked for 15 minutes", a)
	}
	if err := s.PasswordSucceeded(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if a, _ = s.Account(t.Context(), "alice"); a.LockedUntil.After(t0) {
		t.Errorf("after the right password: %+v, want unlocked", a)
	}
	if err := s.PasswordFailed(t.Context(), "nobody", t0, 3, time.Minute); err != nil {
		t.Errorf("a wrong password for an unknown account: %v", err)
	}
}

// Changing a password or deleting an account ends its sessions, and only its.
func TestAccount_ChangesEndSessions(t *testing.T) {
	s, _ := newStore(t)
	newAccount(t, s, "alice")
	newAccount(t, s, "bob")
	session := func(id, username string) {
		t.Helper()
		se := store.Session{Subject: store.AccountSubjectPrefix + username, Username: username, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}
		if err := s.CreateSession(t.Context(), id, se); err != nil {
			t.Fatal(err)
		}
	}
	live := func(id string) bool {
		t.Helper()
		_, err := s.Session(t.Context(), id, t0)
		return err == nil
	}
	session("alice-1", "alice")
	session("bob-1", "bob")
	if err := s.SetPassword(t.Context(), "alice", "hash-2"); err != nil {
		t.Fatal(err)
	}
	if live("alice-1") || !live("bob-1") {
		t.Errorf("after alice's new password: alice's session live %v, bob's %v", live("alice-1"), live("bob-1"))
	}
	if a, _ := s.Account(t.Context(), "alice"); a.PasswordHash != "hash-2" {
		t.Errorf("password hash = %q", a.PasswordHash)
	}
	if err := s.SetRole(t.Context(), "bob", "admin"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Account(t.Context(), "bob"); a.Role != "admin" {
		t.Errorf("bob's role = %q", a.Role)
	}
	if err := s.DeleteAccount(t.Context(), "bob"); err != nil {
		t.Fatal(err)
	}
	if live("bob-1") {
		t.Error("bob's session outlived his account")
	}
	for name, err := range map[string]error{
		"SetPassword":   s.SetPassword(t.Context(), "nobody", "x"),
		"SetRole":       s.SetRole(t.Context(), "nobody", "viewer"),
		"DeleteAccount": s.DeleteAccount(t.Context(), "nobody"),
	} {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s for an unknown account: %v, want ErrNotFound", name, err)
		}
	}
}
