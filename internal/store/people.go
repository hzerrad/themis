package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrAccountExists means an account with that username already exists.
var ErrAccountExists = errors.New("an account with that username already exists")

// AccountSubjectPrefix starts the session subject of someone signed in with
// an account, so their sessions can be told apart from OIDC ones.
const AccountSubjectPrefix = "account:"

// Account is someone who signs in with a username and password.
type Account struct {
	Username     string
	PasswordHash string
	Role         string
	// TemporaryPassword is set while the password is one an admin set: its
	// holder must choose their own before anything else.
	TemporaryPassword bool
	// FailedAttempts counts wrong passwords since the last right one.
	FailedAttempts int
	// LockedUntil is set while too many wrong passwords lock the account.
	LockedUntil time.Time
	CreatedAt   time.Time
	CreatedBy   string
}

const accountColumns = `username, password_hash, role, password_temporary, failed_attempts, coalesce(locked_until, 'epoch'), created_at, created_by`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.Username, &a.PasswordHash, &a.Role, &a.TemporaryPassword, &a.FailedAttempts, &a.LockedUntil, &a.CreatedAt, &a.CreatedBy)
	return a, err
}

// What an account event records.
const (
	AccountCreated       = "account_created"
	AccountDeleted       = "account_deleted"
	RoleChanged          = "role_changed"
	TemporaryPasswordSet = "temporary_password_set"
	PasswordChanged      = "password_changed"
	InitialAdminSignedIn = "initial_admin_signed_in"
	InitialAdminRetired  = "initial_admin_retired"
)

// maxAccountEvents is how many entries of the record AccountEvents returns.
const maxAccountEvents = 200

// AccountEvent is one entry in the record of accounts and of the initial
// admin password: who did what, to which account, and when.
type AccountEvent struct {
	At     time.Time
	Actor  string
	Action string
	// Username is the account the event is about; empty for the initial
	// admin password's.
	Username string
	// Role is the role given, when one is.
	Role string
}

// AccountChange says who changes an account and when, for the record.
type AccountChange struct {
	By string
	At time.Time
	// KeepAnAdmin refuses, with ErrLastAdmin, a change that would leave no
	// admin account.
	KeepAnAdmin bool
}

// ErrLastAdmin means a change would leave no admin account.
var ErrLastAdmin = errors.New("the last admin account")

func recordEvent(ctx context.Context, tx pgx.Tx, e AccountEvent) error {
	if _, err := tx.Exec(ctx, `INSERT INTO account_events (at, actor, action, username, role) VALUES ($1, $2, $3, $4, $5)`,
		e.At, e.Actor, e.Action, e.Username, e.Role); err != nil {
		return fmt.Errorf("record an account event: %w", err)
	}
	return nil
}

// RecordAccountEvent adds e to the record.
func (s *Store) RecordAccountEvent(ctx context.Context, e AccountEvent) error {
	return s.InTx(ctx, func(tx pgx.Tx) error { return recordEvent(ctx, tx, e) })
}

// AccountEvents is the record, newest first: its last 200 entries.
func (s *Store) AccountEvents(ctx context.Context) ([]AccountEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT at, actor, action, username, role FROM account_events ORDER BY at DESC, id DESC LIMIT $1`, maxAccountEvents)
	if err != nil {
		return nil, fmt.Errorf("account events: %w", err)
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByPos[AccountEvent])
	if err != nil {
		return nil, fmt.Errorf("account events: %w", err)
	}
	return events, nil
}

// CreateAccount records a, and that a.CreatedBy made it at a.CreatedAt. A
// taken username is ErrAccountExists.
func (s *Store) CreateAccount(ctx context.Context, a Account) error {
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
	INSERT INTO accounts (username, password_hash, role, password_temporary, created_at, created_by) VALUES ($1, $2, $3, $4, $5, $6)
`, a.Username, a.PasswordHash, a.Role, a.TemporaryPassword, a.CreatedAt, a.CreatedBy); err != nil {
			return err
		}
		return recordEvent(ctx, tx, AccountEvent{At: a.CreatedAt, Actor: a.CreatedBy, Action: AccountCreated, Username: a.Username, Role: a.Role})
	})
	if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
		return ErrAccountExists
	}
	if err != nil {
		return fmt.Errorf("create account: %w", err)
	}
	return nil
}

// Account returns the account with username, or ErrNotFound.
func (s *Store) Account(ctx context.Context, username string) (Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE username = $1`, username))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("account: %w", err)
	}
	return a, nil
}

// Accounts returns every account, by username.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accountColumns+` FROM accounts ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	accounts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Account, error) { return scanAccount(row) })
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	return accounts, nil
}

// HasAccounts reports whether anyone can sign in with a password.
func (s *Store) HasAccounts(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("read accounts: %w", err)
	}
	return exists, nil
}

// PasswordFailed counts a wrong password for username, locking the account
// until now+lockFor once it reaches maxFailures. An unknown username changes
// nothing.
func (s *Store) PasswordFailed(ctx context.Context, username string, now time.Time, maxFailures int, lockFor time.Duration) error {
	if _, err := s.pool.Exec(ctx, `
	UPDATE accounts SET
	  failed_attempts = CASE WHEN failed_attempts + 1 >= $3 THEN 0 ELSE failed_attempts + 1 END,
	  locked_until    = CASE WHEN failed_attempts + 1 >= $3 THEN $2::timestamptz + $4::interval ELSE locked_until END
	WHERE username = $1
`, username, now, maxFailures, lockFor); err != nil {
		return fmt.Errorf("count a wrong password: %w", err)
	}
	return nil
}

// PasswordSucceeded forgets username's wrong passwords.
func (s *Store) PasswordSucceeded(ctx context.Context, username string) error {
	if _, err := s.pool.Exec(ctx, `UPDATE accounts SET failed_attempts = 0, locked_until = NULL WHERE username = $1`, username); err != nil {
		return fmt.Errorf("forget wrong passwords: %w", err)
	}
	return nil
}

// SetPassword replaces username's password, unlocks the account, and ends
// its sessions, so an old password stops working everywhere at once. A
// temporary one must be replaced at the next sign-in.
func (s *Store) SetPassword(ctx context.Context, username, hash string, temporary bool, c AccountChange) error {
	action := PasswordChanged
	if temporary {
		action = TemporaryPasswordSet
	}
	return s.changeAccount(ctx, username, c, action, false,
		`UPDATE accounts SET password_hash = $2, password_temporary = $3, failed_attempts = 0, locked_until = NULL WHERE username = $1`, hash, temporary)
}

// SetRole changes username's role. Their sessions read it on every request,
// so it applies at once.
func (s *Store) SetRole(ctx context.Context, username, role string, c AccountChange) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if role != "admin" {
			if err := keepAnAdmin(ctx, tx, username, c); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE accounts SET role = $2 WHERE username = $1`, username, role)
		if err != nil {
			return fmt.Errorf("set role: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return recordEvent(ctx, tx, AccountEvent{At: c.At, Actor: c.By, Action: RoleChanged, Username: username, Role: role})
	})
}

// DeleteAccount removes username and ends its sessions.
func (s *Store) DeleteAccount(ctx context.Context, username string, c AccountChange) error {
	return s.changeAccount(ctx, username, c, AccountDeleted, true, `DELETE FROM accounts WHERE username = $1`)
}

// changeAccount runs query on username's account, ends its sessions and
// records action, in one transaction. An unknown username is ErrNotFound.
func (s *Store) changeAccount(ctx context.Context, username string, c AccountChange, action string, removesAdmin bool, query string, args ...any) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if removesAdmin {
			if err := keepAnAdmin(ctx, tx, username, c); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, query, append([]any{username}, args...)...)
		if err != nil {
			return fmt.Errorf("change account: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE subject = $1`, AccountSubjectPrefix+username); err != nil {
			return fmt.Errorf("end the account's sessions: %w", err)
		}
		return recordEvent(ctx, tx, AccountEvent{At: c.At, Actor: c.By, Action: action, Username: username})
	})
}

// keepAnAdmin refuses, when c asks, to take the admin role from username if
// no other admin account remains. The admin accounts stay locked until tx
// ends, so two admins removing each other at once can't both succeed.
func keepAnAdmin(ctx context.Context, tx pgx.Tx, username string, c AccountChange) error {
	if !c.KeepAnAdmin {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT username FROM accounts WHERE role = 'admin' FOR UPDATE`)
	if err != nil {
		return fmt.Errorf("read the admin accounts: %w", err)
	}
	admins, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the admin accounts: %w", err)
	}
	if len(admins) == 1 && admins[0] == username {
		return ErrLastAdmin
	}
	return nil
}

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

// SetupFinished reports whether an admin has signed in with an account or
// through the identity provider.
func (s *Store) SetupFinished(ctx context.Context) (bool, error) {
	var finished bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM setup)`).Scan(&finished); err != nil {
		return false, fmt.Errorf("read setup: %w", err)
	}
	return finished, nil
}

// FinishSetup records that by, an admin, signed in at at, which retires the
// initial admin password, and reports whether this call finished it. Setup
// finishes once: later calls change nothing.
func (s *Store) FinishSetup(ctx context.Context, by string, at time.Time) (bool, error) {
	done := false
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO setup (finished_at, finished_by) VALUES ($1, $2) ON CONFLICT DO NOTHING`, at, by)
		if err != nil {
			return err
		}
		if done = tag.RowsAffected() == 1; !done {
			return nil
		}
		return recordEvent(ctx, tx, AccountEvent{At: at, Actor: by, Action: InitialAdminRetired})
	})
	if err != nil {
		return false, fmt.Errorf("finish setup: %w", err)
	}
	return done, nil
}

// InitialAdminSubject is the session subject of whoever signed in with the
// initial admin password.
const InitialAdminSubject = "initial-admin"

// InitialAdminLockedUntil is when wrong tries stop locking the initial admin
// password: a time in the past while they don't.
func (s *Store) InitialAdminLockedUntil(ctx context.Context) (time.Time, error) {
	var until time.Time
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(locked_until, 'epoch') FROM initial_admin`).Scan(&until); err != nil {
		return time.Time{}, fmt.Errorf("read the initial admin lock: %w", err)
	}
	return until, nil
}

// InitialAdminPasswordFailed counts a wrong initial admin password, locking
// it until now+lockFor once it reaches maxFailures, as PasswordFailed does
// for an account.
func (s *Store) InitialAdminPasswordFailed(ctx context.Context, now time.Time, maxFailures int, lockFor time.Duration) error {
	if _, err := s.pool.Exec(ctx, `
	UPDATE initial_admin SET
	  failed_attempts = CASE WHEN failed_attempts + 1 >= $2 THEN 0 ELSE failed_attempts + 1 END,
	  locked_until    = CASE WHEN failed_attempts + 1 >= $2 THEN $1::timestamptz + $3::interval ELSE locked_until END
`, now, maxFailures, lockFor); err != nil {
		return fmt.Errorf("count a wrong initial admin password: %w", err)
	}
	return nil
}

// InitialAdminPasswordSucceeded forgets wrong initial admin passwords.
func (s *Store) InitialAdminPasswordSucceeded(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `UPDATE initial_admin SET failed_attempts = 0, locked_until = NULL`); err != nil {
		return fmt.Errorf("forget wrong initial admin passwords: %w", err)
	}
	return nil
}
