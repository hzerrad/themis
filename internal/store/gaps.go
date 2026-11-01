package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/evidence"
)

// EvidenceGap is evidence an investigation needed and could not have.
type EvidenceGap struct {
	IncidentID string
	// LeaseID is empty for evidence collected before any lease.
	LeaseID  string
	Template string
	Params   map[string]string
	Cause    evidence.GapCause
	// Detail is what Kubernetes or the core said, if anything.
	Detail string
	At     time.Time
}

// InsertEvidenceGap records a gap in tx, so it stands or falls with the work
// that saw it. A gap already recorded for the same lease, template and cause
// is kept as it was.
func (s *Store) InsertEvidenceGap(ctx context.Context, tx pgx.Tx, g EvidenceGap) error {
	params := g.Params
	if params == nil {
		params = map[string]string{}
	}
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var lease *string
	if g.LeaseID != "" {
		lease = &g.LeaseID
	}
	tag, err := tx.Exec(ctx, `
	INSERT INTO evidence_gaps (incident_id, lease_id, template, params, cause, detail, at)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	ON CONFLICT (incident_id, lease_id, template, cause) DO NOTHING
`, g.IncidentID, lease, g.Template, p, g.Cause, g.Detail, g.At)
	if err != nil {
		return fmt.Errorf("record evidence gap of incident %s: %w", g.IncidentID, err)
	}
	if tag.RowsAffected() == 1 {
		AfterCommit(tx, missingEvidenceTotal.WithLabelValues(g.Template, string(g.Cause)).Inc)
	}
	return nil
}

// EvidenceGaps returns an incident's gaps in the order they were recorded.
func (s *Store) EvidenceGaps(ctx context.Context, incidentID string) ([]EvidenceGap, error) {
	rows, err := s.pool.Query(ctx, `
	SELECT incident_id::text, coalesce(lease_id::text, ''), template, params, cause, detail, at
	FROM evidence_gaps WHERE incident_id = $1 ORDER BY id
`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("evidence gaps of incident %s: %w", incidentID, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (EvidenceGap, error) {
		var (
			g      EvidenceGap
			params []byte
		)
		if err := row.Scan(&g.IncidentID, &g.LeaseID, &g.Template, &params, &g.Cause, &g.Detail, &g.At); err != nil {
			return EvidenceGap{}, err
		}
		return g, json.Unmarshal(params, &g.Params)
	})
}
