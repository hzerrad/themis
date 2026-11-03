package store

import (
	"context"
	"fmt"
	"time"
)

// SetupFinished reports whether an admin has signed in through a real sign-in.
func (s *Store) SetupFinished(ctx context.Context) (bool, error) {
	var finished bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM setup)`).Scan(&finished); err != nil {
		return false, fmt.Errorf("read setup: %w", err)
	}
	return finished, nil
}

// FinishSetup records that by, an admin, signed in at at. Setup finishes once:
// later calls change nothing.
func (s *Store) FinishSetup(ctx context.Context, by string, at time.Time) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO setup (finished_at, finished_by) VALUES ($1, $2) ON CONFLICT DO NOTHING`, at, by); err != nil {
		return fmt.Errorf("finish setup: %w", err)
	}
	return nil
}
