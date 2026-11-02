package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SignIn is a sign-in waiting for the identity provider to send the person back.
type SignIn struct {
	State     string
	Nonce     string
	Verifier  string
	ExpiresAt time.Time
}

// Session is a person signed in through the identity provider.
type Session struct {
	Subject   string
	Username  string
	Groups    []string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// StartSignIn records a sign-in, and forgets those that expired.
func (s *Store) StartSignIn(ctx context.Context, in SignIn, now time.Time) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM sign_ins WHERE expires_at <= $1`, now); err != nil {
			return fmt.Errorf("forget expired sign-ins: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sign_ins (state, nonce, verifier, expires_at) VALUES ($1, $2, $3, $4)`,
			in.State, in.Nonce, in.Verifier, in.ExpiresAt); err != nil {
			return fmt.Errorf("record sign-in: %w", err)
		}
		return nil
	})
}

// FinishSignIn returns the sign-in with state and forgets it, so it can be
// used once. An unknown or expired one is ErrNotFound.
func (s *Store) FinishSignIn(ctx context.Context, state string, now time.Time) (SignIn, error) {
	in := SignIn{State: state}
	err := s.pool.QueryRow(ctx, `DELETE FROM sign_ins WHERE state = $1 RETURNING nonce, verifier, expires_at`, state).
		Scan(&in.Nonce, &in.Verifier, &in.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !in.ExpiresAt.After(now) {
		return SignIn{}, ErrNotFound
	}
	if err != nil {
		return SignIn{}, fmt.Errorf("finish sign-in: %w", err)
	}
	return in, nil
}

func hashID(id string) []byte {
	h := sha256.Sum256([]byte(id))
	return h[:]
}

// CreateSession records a session under id, and forgets those that expired.
func (s *Store) CreateSession(ctx context.Context, id string, se Session) error {
	groups := se.Groups
	if groups == nil {
		groups = []string{}
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, se.CreatedAt); err != nil {
			return fmt.Errorf("forget expired sessions: %w", err)
		}
		if _, err := tx.Exec(ctx, `
	INSERT INTO sessions (id_hash, subject, username, groups, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6)
`, hashID(id), se.Subject, se.Username, groups, se.CreatedAt, se.ExpiresAt); err != nil {
			return fmt.Errorf("record session: %w", err)
		}
		return nil
	})
}

// Session returns the live session with id. An unknown or expired one is
// ErrNotFound.
func (s *Store) Session(ctx context.Context, id string, now time.Time) (Session, error) {
	var se Session
	err := s.pool.QueryRow(ctx, `
	SELECT subject, username, groups, created_at, expires_at FROM sessions WHERE id_hash = $1 AND expires_at > $2
`, hashID(id), now).Scan(&se.Subject, &se.Username, &se.Groups, &se.CreatedAt, &se.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("session: %w", err)
	}
	return se, nil
}

// EndSession forgets the session with id, if there is one.
func (s *Store) EndSession(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id_hash = $1`, hashID(id)); err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	return nil
}
