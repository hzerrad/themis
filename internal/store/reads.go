package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Why a read of a source of alerts failed.
const (
	// ReadUnreachable: the source didn't answer.
	ReadUnreachable = "unreachable"
	// ReadRefused: the source refused Themis's credentials.
	ReadRefused = "refused"
	// ReadUnexpected: the source answered, but not as it should.
	ReadUnexpected = "unexpected"
)

// AlertRead is how Themis last read a source of alerts. A zero time means
// never.
type AlertRead struct {
	// Address and Matchers are those of the last successful read.
	Address  string
	Matchers string
	ReadAt   time.Time

	FailedAt    time.Time
	FailureKind string
	Failure     string
}

// LastAlertRead returns how source was last read, or ErrNotFound if never.
func (s *Store) LastAlertRead(ctx context.Context, source string) (AlertRead, error) {
	r, err := scanAlertRead(s.pool.QueryRow(ctx, alertReadQuery, source))
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertRead{}, ErrNotFound
	}
	if err != nil {
		return AlertRead{}, fmt.Errorf("read the last read of %s: %w", source, err)
	}
	return r, nil
}

// LockAlertRead returns how source was last read, locked until tx ends, so
// reads of it apply one at a time.
func (s *Store) LockAlertRead(ctx context.Context, tx pgx.Tx, source string) (AlertRead, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO alert_reads (source) VALUES ($1) ON CONFLICT DO NOTHING`, source); err != nil {
		return AlertRead{}, fmt.Errorf("lock the reads of %s: %w", source, err)
	}
	r, err := scanAlertRead(tx.QueryRow(ctx, alertReadQuery+" FOR UPDATE", source))
	if err != nil {
		return AlertRead{}, fmt.Errorf("lock the reads of %s: %w", source, err)
	}
	return r, nil
}

const alertReadQuery = `SELECT address, matchers, read_at, failed_at, failure_kind, failure FROM alert_reads WHERE source = $1`

func scanAlertRead(row pgx.Row) (AlertRead, error) {
	var r AlertRead
	var readAt, failedAt *time.Time
	if err := row.Scan(&r.Address, &r.Matchers, &readAt, &failedAt, &r.FailureKind, &r.Failure); err != nil {
		return AlertRead{}, err
	}
	if readAt != nil {
		r.ReadAt = *readAt
	}
	if failedAt != nil {
		r.FailedAt = *failedAt
	}
	return r, nil
}

// AlertReadSucceeded records, in tx, that source was read at at.
func (s *Store) AlertReadSucceeded(ctx context.Context, tx pgx.Tx, source, address, matchers string, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE alert_reads SET address = $2, matchers = $3, read_at = $4 WHERE source = $1`, source, address, matchers, at)
	if err != nil {
		return fmt.Errorf("record the read of %s: %w", source, err)
	}
	return nil
}

// AlertReadFailed records that reading source failed at at, of kind, as
// failure says.
func (s *Store) AlertReadFailed(ctx context.Context, source, kind, failure string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
	INSERT INTO alert_reads (source, failed_at, failure_kind, failure) VALUES ($1, $2, $3, $4)
	ON CONFLICT (source) DO UPDATE SET failed_at = excluded.failed_at, failure_kind = excluded.failure_kind, failure = excluded.failure
`, source, at, kind, failure)
	if err != nil {
		return fmt.Errorf("record the failed read of %s: %w", source, err)
	}
	return nil
}

// ReadAlert is an alert Themis took from a source it reads.
type ReadAlert struct {
	SourceID string
	Name     string
	StartsAt time.Time
	// EndsAt is the end the source last gave, while it still fires.
	EndsAt time.Time
	// SeenAt is when it was listed, at least: it isn't written on every read.
	SeenAt time.Time
	// TakenAt is when it was last taken as firing.
	TakenAt time.Time
}

// ReadAlerts returns the alerts taken from source.
func (s *Store) ReadAlerts(ctx context.Context, q Querier, source string) ([]ReadAlert, error) {
	rows, err := q.Query(ctx, `
	SELECT source_id, name, starts_at, ends_at, seen_at, taken_at FROM read_alerts WHERE source = $1 ORDER BY source_id
`, source)
	if err != nil {
		return nil, fmt.Errorf("read the alerts taken from %s: %w", source, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ReadAlert])
	if err != nil {
		return nil, fmt.Errorf("read the alerts taken from %s: %w", source, err)
	}
	return out, nil
}

// PutReadAlerts saves put and forgets the alerts named in drop, in tx.
func (s *Store) PutReadAlerts(ctx context.Context, tx pgx.Tx, source string, put []ReadAlert, drop []string) error {
	if len(drop) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM read_alerts WHERE source = $1 AND source_id = ANY($2)`, source, drop); err != nil {
			return fmt.Errorf("forget alerts taken from %s: %w", source, err)
		}
	}
	if len(put) == 0 {
		return nil
	}
	ids, names := make([]string, len(put)), make([]string, len(put))
	starts, ends, seen, taken := make([]time.Time, len(put)), make([]time.Time, len(put)), make([]time.Time, len(put)), make([]time.Time, len(put))
	for i, a := range put {
		ids[i], names[i], starts[i], ends[i], seen[i], taken[i] = a.SourceID, a.Name, a.StartsAt, a.EndsAt, a.SeenAt, a.TakenAt
	}
	_, err := tx.Exec(ctx, `
	INSERT INTO read_alerts (source, source_id, name, starts_at, ends_at, seen_at, taken_at)
	SELECT $1, * FROM unnest($2::text[], $3::text[], $4::timestamptz[], $5::timestamptz[], $6::timestamptz[], $7::timestamptz[])
	ON CONFLICT (source, source_id) DO UPDATE
	SET name = excluded.name, starts_at = excluded.starts_at, ends_at = excluded.ends_at,
	    seen_at = excluded.seen_at, taken_at = excluded.taken_at
`, source, ids, names, starts, ends, seen, taken)
	if err != nil {
		return fmt.Errorf("save alerts taken from %s: %w", source, err)
	}
	return nil
}
