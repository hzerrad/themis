package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AppliedFix is a fix Themis applied, as its incident recorded it.
type AppliedFix struct {
	Action Action
	// AppliedAt is when the executor finished applying it, and Done what it
	// said it did.
	AppliedAt time.Time
	Done      string
	// Outcome is how its verification ended; empty while it runs, or when
	// it never started.
	Outcome string
}

// AppliedFixLeaving returns the latest fix Themis applied to the target
// with uid that left it with the template mark names: the change the
// target runs, if it is Themis's. A restore isn't one: it puts back what a
// person left. ErrNotFound when there is none.
func (s *Store) AppliedFixLeaving(ctx context.Context, uid, mark string) (AppliedFix, error) {
	var f AppliedFix
	var id string
	err := s.pool.QueryRow(ctx, `
	SELECT a.id::text, e.finished_at, e.message, coalesce(v.outcome, '')
	FROM actions a
	JOIN executions e ON e.action_id = a.id AND e.status = 'succeeded'
	LEFT JOIN verifications v ON v.action_id = a.id
	WHERE a.target_uid = $1 AND a.mark = $2 AND a.state = 'succeeded' AND NOT (a.params ? 'restores')
	ORDER BY e.finished_at DESC, a.id DESC LIMIT 1
`, uid, mark).Scan(&id, &f.AppliedAt, &f.Done, &f.Outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppliedFix{}, fmt.Errorf("fix leaving %s on %s: %w", mark, uid, ErrNotFound)
	}
	if err != nil {
		return AppliedFix{}, fmt.Errorf("fix leaving %s on %s: %w", mark, uid, err)
	}
	if f.Action, err = s.Action(ctx, s.DB(), id); err != nil {
		return AppliedFix{}, err
	}
	return f, nil
}
