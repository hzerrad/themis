package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/incident"
)

var (
	ErrStateConflict     = errors.New("incident state conflict")
	ErrIllegalTransition = errors.New("illegal incident state transition")
)

type IncidentArgs struct {
	Fingerprint string
	Profile     string
	Target      incident.TargetRef
	Payload     []byte
}

type TransitionArgs struct {
	ID     string
	From   incident.State
	To     incident.State
	Reason string
	Actor  string
}

func (s *Store) CreateOrJoinIncident(ctx context.Context, tx pgx.Tx, in IncidentArgs) (string, bool, error) {
	newID, err := uuid.NewV7()
	if err != nil {
		return "", false, fmt.Errorf("failed to create incident ID: %w", err)
	}

	sql := `
	INSERT INTO incidents (id, fingerprint, state, profile, target_group, target_kind, target_namespace, target_name, target_uid)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	ON CONFLICT (fingerprint) WHERE state NOT IN ('resolved', 'contained_escalated', 'escalated', 'closed_no_action', 'self_resolved', 'rejected', 'canceled')
	DO UPDATE SET occurrence_count = incidents.occurrence_count + 1, updated_at = now()
	RETURNING id::text
`

	row := tx.QueryRow(ctx, sql, newID, in.Fingerprint, incident.Received,
		in.Profile, in.Target.Group, in.Target.Kind, in.Target.Namespace,
		in.Target.Name, in.Target.UID,
	)

	var id string
	err = row.Scan(&id)
	if err != nil {
		return "", false, fmt.Errorf("failed to scan incident ID: %w", err)
	}

	oid, err := uuid.NewV7()
	if err != nil {
		return "", false, fmt.Errorf("failed to generate occurrence ID: %w", err)
	}

	sql = `
	INSERT INTO occurrences (id, incident_id, payload)
	VALUES ($1, $2, $3)
`
	_, err = tx.Exec(ctx, sql, oid, id, in.Payload)
	if err != nil {
		return "", false, fmt.Errorf("failed to record occurrence: %w", err)
	}

	return id, newID.String() == id, nil
}

func (s *Store) Transition(ctx context.Context, tx pgx.Tx, tst TransitionArgs) error {
	if !incident.CanTransition(tst.From, tst.To) {
		return fmt.Errorf("transition %s %s->%s: %w", tst.ID, tst.From, tst.To, ErrIllegalTransition)
	}

	sql := `
	UPDATE incidents
	SET state = $1, updated_at = now()
	WHERE id = $2 AND state = $3
`
	tag, err := tx.Exec(ctx, sql, tst.To, tst.ID, tst.From)
	if err != nil {
		return fmt.Errorf("transition %s %s->%s: %w", tst.ID, tst.From, tst.To, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("transition %s %s->%s: %w", tst.ID, tst.From, tst.To, ErrStateConflict)
	}

	sql = `
	INSERT INTO transitions (incident_id, from_state, to_state, reason, actor)
	VALUES ($1, $2, $3, $4, $5)
`
	_, err = tx.Exec(ctx, sql, tst.ID, tst.From, tst.To, tst.Reason, tst.Actor)
	if err != nil {
		return fmt.Errorf("transition %s %s->%s: %w", tst.ID, tst.From, tst.To, err)
	}

	return nil
}
