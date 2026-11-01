package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"go.opentelemetry.io/otel/trace"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/telemetry"
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

	// The last transition entered tst.From; updated_at also moves when an
	// alert joins, so it cannot say how long the incident spent there.
	sql := `
	WITH prev AS (
		SELECT i.created_at,
		       coalesce((SELECT at FROM transitions t WHERE t.incident_id = i.id ORDER BY t.id DESC LIMIT 1), i.created_at) AS entered
		FROM incidents i WHERE i.id = $2 AND i.state = $3 FOR UPDATE OF i
	)
	UPDATE incidents i
	SET state = $1, updated_at = now()
	FROM prev
	WHERE i.id = $2 AND i.state = $3
	RETURNING greatest(extract(epoch FROM now() - prev.entered), 0)::float8, greatest(extract(epoch FROM now() - prev.created_at), 0)::float8
`
	var sinceChange, sinceCreated float64
	err := tx.QueryRow(ctx, sql, tst.To, tst.ID, tst.From).Scan(&sinceChange, &sinceCreated)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("transition %s %s->%s: %w", tst.ID, tst.From, tst.To, ErrStateConflict)
	}
	if err != nil {
		return fmt.Errorf("transition %s %s->%s: %w", tst.ID, tst.From, tst.To, err)
	}
	AfterCommit(tx, func() {
		observeTransition(tst.From, tst.To, sinceChange, sinceCreated)
		// Never the reason: an investigation's summary, written by the model, can be one.
		slog.InfoContext(telemetry.WithAttrs(ctx, slog.String(telemetry.FieldIncidentID, tst.ID)),
			"incident transitioned", "from", tst.From, "to", tst.To, "actor", tst.Actor)
	})

	var traceID *string
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		traceID = new(sc.TraceID().String())
	}
	sql = `
	INSERT INTO transitions (incident_id, from_state, to_state, reason, actor, trace_id)
	VALUES ($1, $2, $3, $4, $5, $6)
`
	_, err = tx.Exec(ctx, sql, tst.ID, tst.From, tst.To, tst.Reason, tst.Actor, traceID)
	if err != nil {
		return fmt.Errorf("transition %s %s->%s: %w", tst.ID, tst.From, tst.To, err)
	}

	if tst.To == incident.Escalated || tst.To == incident.ContainedEscalated {
		return s.EnqueueTx(ctx, tx, EscalationArgs{IncidentID: tst.ID, From: tst.From, To: tst.To, Reason: tst.Reason, At: time.Now().UTC()})
	}
	return nil
}

// EscalationArgs notify that an incident was handed to humans. They are
// queued by the transition itself, so no escalation goes unannounced.
type EscalationArgs struct {
	IncidentID string         `json:"incidentID"`
	From       incident.State `json:"from"`
	To         incident.State `json:"to"`
	Reason     string         `json:"reason"`
	At         time.Time      `json:"at"`
}

// Kind is persisted with each job and must never change.
func (EscalationArgs) Kind() string { return "notify_escalation" }

func (a EscalationArgs) Incident() string { return a.IncidentID }

// InsertOpts keep trying a receiver for about a day.
func (EscalationArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{MaxAttempts: 12} }

// ErrNotFound means no row has the requested ID.
var ErrNotFound = errors.New("not found")

type Incident struct {
	ID              string
	State           incident.State
	Profile         string
	Target          incident.TargetRef
	OccurrenceCount int
	CreatedAt       time.Time
	UpdatedAt       time.Time

	// ObservedSpecHash is the template the investigation started from.
	ObservedSpecHash string

	// SupersededBy is the incident that replaced this one after the target
	// changed outside Themis.
	SupersededBy string
}

const incidentColumns = `id::text, state, profile, target_group, target_kind, target_namespace, target_name, target_uid, occurrence_count, created_at, updated_at,
	observed_spec_hash, coalesce(superseded_by::text, '')`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	t := &in.Target
	err := row.Scan(&in.ID, &in.State, &in.Profile, &t.Group, &t.Kind, &t.Namespace, &t.Name, &t.UID, &in.OccurrenceCount, &in.CreatedAt, &in.UpdatedAt,
		&in.ObservedSpecHash, &in.SupersededBy)
	return in, err
}

func (s *Store) Incident(ctx context.Context, id string) (Incident, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Incident{}, fmt.Errorf("incident %q: %w", id, ErrNotFound)
	}
	in, err := scanIncident(s.pool.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, fmt.Errorf("incident %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Incident{}, fmt.Errorf("incident %s: %w", id, err)
	}
	return in, nil
}

// ListIncidents returns the most recent incidents, newest first.
func (s *Store) ListIncidents(ctx context.Context, limit int) ([]Incident, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+incidentColumns+` FROM incidents ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list incidents: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Incident, error) { return scanIncident(row) })
}

type Transition struct {
	From   incident.State
	To     incident.State
	Reason string
	Actor  string
	At     time.Time

	// TraceID is the trace of the work that made the change, if it was traced.
	TraceID string
}

// Transitions returns an incident's audit trail in order.
func (s *Store) Transitions(ctx context.Context, incidentID string) ([]Transition, error) {
	rows, err := s.pool.Query(ctx, `SELECT from_state, to_state, reason, actor, at, coalesce(trace_id, '') FROM transitions WHERE incident_id = $1 ORDER BY id`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("transitions of incident %s: %w", incidentID, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Transition, error) {
		var t Transition
		err := row.Scan(&t.From, &t.To, &t.Reason, &t.Actor, &t.At, &t.TraceID)
		return t, err
	})
}

// SetObservedSpec records the template an investigation starts from.
func (s *Store) SetObservedSpec(ctx context.Context, tx pgx.Tx, id, hash string) error {
	if _, err := tx.Exec(ctx, `UPDATE incidents SET observed_spec_hash = $2 WHERE id = $1`, id, hash); err != nil {
		return fmt.Errorf("record observed spec of incident %s: %w", id, err)
	}
	return nil
}

// ReconcilableIncidents are open incidents not yet executing whose template
// is known, oldest first.
func (s *Store) ReconcilableIncidents(ctx context.Context, limit int) ([]Incident, error) {
	rows, err := s.pool.Query(ctx, `
	SELECT `+incidentColumns+` FROM incidents
	WHERE state IN ('investigating', 'diagnosed', 'proposed', 'awaiting_approval', 'authorized') AND observed_spec_hash <> ''
	ORDER BY created_at LIMIT $1
`, limit)
	if err != nil {
		return nil, fmt.Errorf("list incidents to reconcile: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Incident, error) { return scanIncident(row) })
}

// Supersede links an incident to the one that replaced it, and carries its
// firing alerts over, so they resolve the successor.
func (s *Store) Supersede(ctx context.Context, tx pgx.Tx, oldID, newID string) error {
	if _, err := tx.Exec(ctx, `UPDATE incidents SET superseded_by = $2 WHERE id = $1`, oldID, newID); err != nil {
		return fmt.Errorf("supersede incident %s: %w", oldID, err)
	}
	_, err := tx.Exec(ctx, `
	INSERT INTO incident_alerts (incident_id, source, source_id, status, starts_at)
	SELECT $2, source, source_id, status, starts_at FROM incident_alerts WHERE incident_id = $1 AND status = 'firing'
	ON CONFLICT DO NOTHING
`, oldID, newID)
	if err != nil {
		return fmt.Errorf("carry alerts of incident %s over: %w", oldID, err)
	}
	return nil
}

// LockIncident returns an incident's state, holding it still until tx ends.
func (s *Store) LockIncident(ctx context.Context, tx pgx.Tx, id string) (incident.State, error) {
	var state incident.State
	err := tx.QueryRow(ctx, `SELECT state FROM incidents WHERE id = $1 FOR UPDATE`, id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("incident %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("lock incident %s: %w", id, err)
	}
	return state, nil
}

// OpenIncidents returns the incidents in the given namespaces that can still
// take a suggestion, newest first. No namespaces means none.
func (s *Store) OpenIncidents(ctx context.Context, namespaces []string, limit int) ([]Incident, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+incidentColumns+` FROM incidents
	WHERE state IN ('investigating', 'inconclusive') AND target_namespace = ANY($1)
	ORDER BY created_at DESC, id DESC LIMIT $2`, namespaces, limit)
	if err != nil {
		return nil, fmt.Errorf("open incidents: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Incident, error) { return scanIncident(row) })
}

// UndecidedBefore returns the incidents left undecided since before t. The
// last transition says since when; updated_at also moves when an alert joins.
func (s *Store) UndecidedBefore(ctx context.Context, q Querier, t time.Time, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT i.id::text FROM incidents i
	WHERE i.state = 'inconclusive'
	  AND (SELECT tr.at FROM transitions tr WHERE tr.incident_id = i.id ORDER BY tr.id DESC LIMIT 1) <= $1
	ORDER BY i.id LIMIT $2`, t, limit)
	if err != nil {
		return nil, fmt.Errorf("undecided incidents: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
