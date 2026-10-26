package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/incident"
)

// FiringAlertArgs identify an alert that is firing for an incident.
type FiringAlertArgs struct {
	IncidentID string

	Source   string
	SourceID string
	StartsAt time.Time
}

// RecordFiringAlert records that an alert is firing for an incident, unless
// the recorded notification is newer.
func (s *Store) RecordFiringAlert(ctx context.Context, tx pgx.Tx, in FiringAlertArgs) error {
	sql := `
	INSERT INTO incident_alerts (incident_id, source, source_id, status, starts_at)
	VALUES ($1, $2, $3, 'firing', $4)
	ON CONFLICT (incident_id, source, source_id) DO UPDATE
	SET status = 'firing', starts_at = EXCLUDED.starts_at, ends_at = NULL, updated_at = now()
	WHERE EXCLUDED.starts_at > incident_alerts.starts_at
	   OR (EXCLUDED.starts_at = incident_alerts.starts_at AND incident_alerts.status = 'firing')
`
	if _, err := tx.Exec(ctx, sql, in.IncidentID, in.Source, in.SourceID, in.StartsAt); err != nil {
		return fmt.Errorf("record firing alert %s/%s: %w", in.Source, in.SourceID, err)
	}
	return nil
}

// ResolvedAlertArgs identify an alert that stopped firing.
type ResolvedAlertArgs struct {
	Source   string
	SourceID string
	StartsAt time.Time
	EndsAt   time.Time
}

// AlertIncident is an open incident of a resolved alert.
type AlertIncident struct {
	IncidentID string
	State      incident.State
	Firing     int
}

// ResolveAlert marks an alert resolved in its open incidents and returns them
// with their firing counts. The incidents stay locked for the transaction, so
// the counts hold.
func (s *Store) ResolveAlert(ctx context.Context, tx pgx.Tx, in ResolvedAlertArgs) ([]AlertIncident, error) {
	terminal := make([]string, 0, len(incident.TerminalStates()))
	for _, st := range incident.TerminalStates() {
		terminal = append(terminal, string(st))
	}

	rows, err := tx.Query(ctx, `
	SELECT i.id::text, i.state::text
	FROM incidents i
	JOIN incident_alerts ia ON ia.incident_id = i.id
	WHERE ia.source = $1 AND ia.source_id = $2 AND i.state::text <> ALL($3::text[])
	ORDER BY i.id
	FOR UPDATE OF i
`, in.Source, in.SourceID, terminal)
	if err != nil {
		return nil, fmt.Errorf("find incidents of alert %s/%s: %w", in.Source, in.SourceID, err)
	}
	incidents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AlertIncident, error) {
		var a AlertIncident
		err := row.Scan(&a.IncidentID, &a.State)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("find incidents of alert %s/%s: %w", in.Source, in.SourceID, err)
	}

	for i := range incidents {
		id := incidents[i].IncidentID
		_, err := tx.Exec(ctx, `
	UPDATE incident_alerts
	SET status = 'resolved', ends_at = $5, updated_at = now()
	WHERE incident_id = $1 AND source = $2 AND source_id = $3
	  AND status = 'firing' AND starts_at <= $4
`, id, in.Source, in.SourceID, in.StartsAt, in.EndsAt)
		if err != nil {
			return nil, fmt.Errorf("resolve alert %s/%s in incident %s: %w", in.Source, in.SourceID, id, err)
		}

		err = tx.QueryRow(ctx,
			`SELECT count(*) FROM incident_alerts WHERE incident_id = $1 AND status = 'firing'`, id,
		).Scan(&incidents[i].Firing)
		if err != nil {
			return nil, fmt.Errorf("count firing alerts of incident %s: %w", id, err)
		}
	}
	return incidents, nil
}
