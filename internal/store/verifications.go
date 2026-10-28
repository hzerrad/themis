package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Verification is the persisted progress of an action's verification.
type Verification struct {
	ActionID     string
	WindowStart  time.Time
	ReadyBefore  int32
	RestartsBase int32
	HealthySince *time.Time
	Outcome      string
	Observation  []byte
}

// StartVerification records the baseline once and returns the stored
// verification, so a rerun keeps the first baseline.
func (s *Store) StartVerification(ctx context.Context, tx pgx.Tx, v Verification) (Verification, error) {
	_, err := tx.Exec(ctx, `
	INSERT INTO verifications (action_id, window_start, ready_before, restarts_base)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (action_id) DO NOTHING
`, v.ActionID, v.WindowStart, v.ReadyBefore, v.RestartsBase)
	if err != nil {
		return Verification{}, fmt.Errorf("start verification of %s: %w", v.ActionID, err)
	}
	return s.Verification(ctx, tx, v.ActionID)
}

func (s *Store) Verification(ctx context.Context, q Querier, actionID string) (Verification, error) {
	var v Verification
	err := q.QueryRow(ctx, `
	SELECT action_id::text, window_start, ready_before, restarts_base, healthy_since, outcome, observation
	FROM verifications WHERE action_id = $1
`, actionID).Scan(&v.ActionID, &v.WindowStart, &v.ReadyBefore, &v.RestartsBase, &v.HealthySince, &v.Outcome, &v.Observation)
	if errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, fmt.Errorf("verification of %s: %w", actionID, ErrNotFound)
	}
	if err != nil {
		return Verification{}, fmt.Errorf("verification of %s: %w", actionID, err)
	}
	return v, nil
}

// UpdateVerification records the latest observation and, once final, the
// outcome.
func (s *Store) UpdateVerification(ctx context.Context, tx pgx.Tx, v Verification) error {
	_, err := tx.Exec(ctx, `
	UPDATE verifications SET healthy_since = $2, outcome = $3, observation = $4, updated_at = now()
	WHERE action_id = $1
`, v.ActionID, v.HealthySince, v.Outcome, jsonOr(v.Observation, "{}"))
	if err != nil {
		return fmt.Errorf("update verification of %s: %w", v.ActionID, err)
	}
	return nil
}
