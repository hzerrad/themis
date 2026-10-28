package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
)

// MaxInvestigationAttempts bounds the leases of one investigation.
const MaxInvestigationAttempts = 3

// ErrLeaseLost means a lease is unknown, expired or superseded: its holder's
// results are stale.
var ErrLeaseLost = errors.New("investigation lease lost")

// Investigation is an incident's investigation and its current lease.
type Investigation struct {
	ID            string
	IncidentID    string
	Profile       string
	Target        incident.TargetRef
	LeaseID       string
	LeaseDeadline time.Time
	Attempts      int
}

// InvestigationResult is what an investigation concluded, as JSON documents.
type InvestigationResult struct {
	Conclusion string
	Summary    string
	Hypotheses []byte
	Usage      []byte
	Provenance []byte
}

// CreateInvestigation records an incident's initial evidence and predicates
// and queues its investigation for leasing.
func (s *Store) CreateInvestigation(ctx context.Context, tx pgx.Tx, incidentID string, items []evidence.Item, results []predicate.Result) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("failed to create investigation ID: %w", err)
	}
	for _, item := range items {
		if err := s.InsertEvidence(ctx, tx, incidentID, "", nil, item); err != nil {
			return "", err
		}
	}
	for _, r := range results {
		ids := r.EvidenceIDs
		if ids == nil {
			ids = []string{}
		}
		_, err := tx.Exec(ctx, `
	INSERT INTO predicate_results (incident_id, name, version, value, reason, evidence_ids)
	VALUES ($1, $2, $3, $4, $5, $6)
`, incidentID, r.Name, r.Version, r.Value.String(), r.Reason, ids)
		if err != nil {
			return "", fmt.Errorf("record predicate %s: %w", r.Name, err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO investigations (id, incident_id) VALUES ($1, $2)`, id, incidentID); err != nil {
		return "", fmt.Errorf("create investigation of incident %s: %w", incidentID, err)
	}
	return id.String(), nil
}

const investigationColumns = `
	v.id::text, v.incident_id::text, i.profile, i.target_group, i.target_kind, i.target_namespace, i.target_name, i.target_uid,
	coalesce(v.lease_id::text, ''), coalesce(v.lease_deadline, 'epoch'), v.attempts`

func scanInvestigation(row pgx.Row) (Investigation, error) {
	var inv Investigation
	t := &inv.Target
	err := row.Scan(&inv.ID, &inv.IncidentID, &inv.Profile, &t.Group, &t.Kind, &t.Namespace, &t.Name, &t.UID,
		&inv.LeaseID, &inv.LeaseDeadline, &inv.Attempts)
	return inv, err
}

// LeaseInvestigation leases the oldest pending investigation, or one whose
// lease expired with attempts left, until now+leaseFor. It returns false when
// there is none.
func (s *Store) LeaseInvestigation(ctx context.Context, tx pgx.Tx, workerID string, now time.Time, leaseFor time.Duration) (Investigation, bool, error) {
	leaseID, err := uuid.NewV7()
	if err != nil {
		return Investigation{}, false, fmt.Errorf("failed to create lease ID: %w", err)
	}
	row := tx.QueryRow(ctx, `
	WITH next AS (
		SELECT id FROM investigations
		WHERE (state = 'pending' OR (state = 'leased' AND lease_deadline < $1)) AND attempts < $2
		ORDER BY created_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	), v AS (
		UPDATE investigations
		SET state = 'leased', lease_id = $3, lease_deadline = $1::timestamptz + $4::interval,
		    worker_id = $5, attempts = attempts + 1, updated_at = now()
		WHERE id = (SELECT id FROM next)
		RETURNING *
	)
	SELECT `+investigationColumns+`
	FROM v JOIN incidents i ON i.id = v.incident_id
`, now, MaxInvestigationAttempts, leaseID, leaseFor, workerID)
	inv, err := scanInvestigation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Investigation{}, false, nil
	}
	if err != nil {
		return Investigation{}, false, fmt.Errorf("lease investigation: %w", err)
	}
	return inv, true, nil
}

// LeasedInvestigation returns the investigation a live lease holds, locked
// for the rest of tx, or ErrLeaseLost.
func (s *Store) LeasedInvestigation(ctx context.Context, tx pgx.Tx, leaseID string, now time.Time) (Investigation, error) {
	if _, err := uuid.Parse(leaseID); err != nil {
		return Investigation{}, ErrLeaseLost
	}
	row := tx.QueryRow(ctx, `
	SELECT `+investigationColumns+`
	FROM investigations v JOIN incidents i ON i.id = v.incident_id
	WHERE v.lease_id = $1 AND v.state = 'leased' AND v.lease_deadline >= $2
	FOR UPDATE OF v
`, leaseID, now)
	inv, err := scanInvestigation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Investigation{}, ErrLeaseLost
	}
	if err != nil {
		return Investigation{}, fmt.Errorf("find lease %s: %w", leaseID, err)
	}
	return inv, nil
}

// RenewLease extends a live lease to now+leaseFor.
func (s *Store) RenewLease(ctx context.Context, tx pgx.Tx, leaseID string, now time.Time, leaseFor time.Duration) (time.Time, error) {
	if _, err := s.LeasedInvestigation(ctx, tx, leaseID, now); err != nil {
		return time.Time{}, err
	}
	var deadline time.Time
	err := tx.QueryRow(ctx, `
	UPDATE investigations SET lease_deadline = $2::timestamptz + $3::interval, updated_at = now()
	WHERE lease_id = $1
	RETURNING lease_deadline
`, leaseID, now, leaseFor).Scan(&deadline)
	if err != nil {
		return time.Time{}, fmt.Errorf("renew lease %s: %w", leaseID, err)
	}
	return deadline, nil
}

// CompleteInvestigation records the result of a leased investigation. The
// caller checks the lease first, in the same transaction.
func (s *Store) CompleteInvestigation(ctx context.Context, tx pgx.Tx, leaseID string, r InvestigationResult) error {
	tag, err := tx.Exec(ctx, `
	UPDATE investigations
	SET state = 'completed', conclusion = $2, summary = $3, hypotheses = $4, usage = $5, provenance = $6, updated_at = now()
	WHERE lease_id = $1 AND state = 'leased'
`, leaseID, r.Conclusion, r.Summary, jsonOr(r.Hypotheses, "[]"), jsonOr(r.Usage, "{}"), jsonOr(r.Provenance, "{}"))
	if err != nil {
		return fmt.Errorf("complete investigation of lease %s: %w", leaseID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// FailInvestigation releases a lease after a failure. The investigation is
// leased again unless that was its last attempt, which it reports.
func (s *Store) FailInvestigation(ctx context.Context, tx pgx.Tx, leaseID, reason, message string) (bool, error) {
	var exhausted bool
	err := tx.QueryRow(ctx, `
	UPDATE investigations
	SET state = CASE WHEN attempts >= $2 THEN 'failed'::investigation_state ELSE 'pending' END,
	    lease_id = NULL, lease_deadline = NULL, failure = $3, updated_at = now()
	WHERE lease_id = $1 AND state = 'leased'
	RETURNING state = 'failed'
`, leaseID, MaxInvestigationAttempts, reason+": "+message).Scan(&exhausted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLeaseLost
	}
	if err != nil {
		return false, fmt.Errorf("fail investigation of lease %s: %w", leaseID, err)
	}
	return exhausted, nil
}

// ExpireInvestigations gives up on expired leases that used the last attempt,
// and returns their incidents.
func (s *Store) ExpireInvestigations(ctx context.Context, tx pgx.Tx, now time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `
	UPDATE investigations
	SET state = 'failed', failure = 'lease expired on the last attempt', lease_id = NULL, updated_at = now()
	WHERE state = 'leased' AND lease_deadline < $1 AND attempts >= $2
	RETURNING incident_id::text
`, now, MaxInvestigationAttempts)
	if err != nil {
		return nil, fmt.Errorf("expire investigations: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("expire investigations: %w", err)
	}
	return ids, nil
}

// InsertEvidence records an item collected for an incident, under a lease
// when an investigator asked for it.
func (s *Store) InsertEvidence(ctx context.Context, tx pgx.Tx, incidentID, leaseID string, params map[string]string, item evidence.Item) error {
	if params == nil {
		params = map[string]string{}
	}
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var lease *string
	if leaseID != "" {
		lease = &leaseID
	}
	_, err = tx.Exec(ctx, `
	INSERT INTO evidence_items (id, incident_id, lease_id, template, params, data, truncated, original_bytes, redactions, classification, collected_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
`, item.ID, incidentID, lease, item.Template, p, item.Data, item.Truncated, item.OriginalBytes, item.Redactions, item.Classification, item.CollectedAt)
	if err != nil {
		return fmt.Errorf("record evidence %s: %w", item.ID, err)
	}
	return nil
}

// Evidence returns an incident's evidence in collection order.
func (s *Store) Evidence(ctx context.Context, incidentID string) ([]evidence.Item, error) {
	rows, err := s.pool.Query(ctx, `
	SELECT e.id::text, e.template, e.data, e.truncated, e.original_bytes, e.redactions, e.classification, e.collected_at,
	       i.target_group, i.target_kind, i.target_namespace, i.target_name, i.target_uid
	FROM evidence_items e JOIN incidents i ON i.id = e.incident_id
	WHERE e.incident_id = $1
	ORDER BY e.collected_at, e.id
`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("evidence of incident %s: %w", incidentID, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (evidence.Item, error) {
		var it evidence.Item
		t := &it.Target
		err := row.Scan(&it.ID, &it.Template, &it.Data, &it.Truncated, &it.OriginalBytes, &it.Redactions, &it.Classification, &it.CollectedAt,
			&t.Group, &t.Kind, &t.Namespace, &t.Name, &t.UID)
		return it, err
	})
}

// CountLeaseEvidence counts the items collected under a lease.
func (s *Store) CountLeaseEvidence(ctx context.Context, tx pgx.Tx, leaseID string) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM evidence_items WHERE lease_id = $1`, leaseID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count evidence of lease %s: %w", leaseID, err)
	}
	return n, nil
}

// MissingEvidence returns the IDs that name no evidence of the incident.
func (s *Store) MissingEvidence(ctx context.Context, tx pgx.Tx, incidentID string, ids []string) ([]string, error) {
	var missing, valid []string
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			missing = append(missing, id)
		} else {
			valid = append(valid, id)
		}
	}
	rows, err := tx.Query(ctx, `
	SELECT wanted FROM unnest($2::text[]) AS wanted
	WHERE NOT EXISTS (SELECT 1 FROM evidence_items e WHERE e.incident_id = $1 AND e.id = wanted::uuid)
`, incidentID, valid)
	if err != nil {
		return nil, fmt.Errorf("check evidence of incident %s: %w", incidentID, err)
	}
	unknown, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("check evidence of incident %s: %w", incidentID, err)
	}
	return append(missing, unknown...), nil
}

// Predicates returns an incident's predicate results.
func (s *Store) Predicates(ctx context.Context, incidentID string) ([]predicate.Result, error) {
	rows, err := s.pool.Query(ctx, `
	SELECT name, version, value::text, reason, evidence_ids::text[]
	FROM predicate_results WHERE incident_id = $1 ORDER BY id
`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("predicates of incident %s: %w", incidentID, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (predicate.Result, error) {
		var (
			r     predicate.Result
			value string
		)
		err := row.Scan(&r.Name, &r.Version, &value, &r.Reason, &r.EvidenceIDs)
		r.Value = predicate.ParseTruth(value)
		return r, err
	})
}

func jsonOr(b []byte, empty string) []byte {
	if len(b) == 0 {
		return []byte(empty)
	}
	return b
}
