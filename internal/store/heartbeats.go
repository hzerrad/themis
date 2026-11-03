package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Components that report they're alive.
const (
	HeartbeatInvestigator = "investigator"
	HeartbeatAlertmanager = "alertmanager"
	// HeartbeatAlertmanagerRefused is a webhook refused for its token.
	HeartbeatAlertmanagerRefused = "alertmanager_refused"
)

// Heartbeat is when a component was last heard from.
type Heartbeat struct {
	SeenAt time.Time
	Detail string
}

// Beat records that component was heard from at at, with detail.
func (s *Store) Beat(ctx context.Context, component, detail string, at time.Time) error {
	if _, err := s.pool.Exec(ctx, `
	INSERT INTO heartbeats (component, seen_at, detail) VALUES ($1, $2, $3)
	ON CONFLICT (component) DO UPDATE SET seen_at = greatest(heartbeats.seen_at, excluded.seen_at), detail = excluded.detail
`, component, at, detail); err != nil {
		return fmt.Errorf("record heartbeat of %s: %w", component, err)
	}
	return nil
}

// LastHeartbeat is when component was last heard from, or ErrNotFound if never.
func (s *Store) LastHeartbeat(ctx context.Context, component string) (Heartbeat, error) {
	var h Heartbeat
	err := s.pool.QueryRow(ctx, `SELECT seen_at, detail FROM heartbeats WHERE component = $1`, component).Scan(&h.SeenAt, &h.Detail)
	if errors.Is(err, pgx.ErrNoRows) {
		return Heartbeat{}, ErrNotFound
	}
	if err != nil {
		return Heartbeat{}, fmt.Errorf("read heartbeat of %s: %w", component, err)
	}
	return h, nil
}

// InvestigationOutcome is how the latest investigation attempt that finished
// went: it completed, or it failed with a kind.
type InvestigationOutcome struct {
	Completed   bool
	FailureKind string
	Failure     string
	Model       string
	At          time.Time
}

// LastInvestigationOutcome returns the latest finished attempt, or
// ErrNotFound if none has finished.
func (s *Store) LastInvestigationOutcome(ctx context.Context) (InvestigationOutcome, error) {
	var o InvestigationOutcome
	err := s.pool.QueryRow(ctx, `
	SELECT state = 'completed', coalesce(failure_kind, ''), coalesce(failure, ''), coalesce(provenance->>'model', ''), updated_at
	FROM investigations
	WHERE state = 'completed' OR failure_kind IS NOT NULL
	ORDER BY updated_at DESC LIMIT 1
`).Scan(&o.Completed, &o.FailureKind, &o.Failure, &o.Model, &o.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvestigationOutcome{}, ErrNotFound
	}
	if err != nil {
		return InvestigationOutcome{}, fmt.Errorf("read the last investigation: %w", err)
	}
	return o, nil
}
