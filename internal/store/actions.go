package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/digest"
)

type ActionState string

const (
	ActionAwaitingApproval ActionState = "awaiting_approval"
	ActionAuthorized       ActionState = "authorized"
	ActionExecuting        ActionState = "executing"
	ActionSucceeded        ActionState = "succeeded"
	ActionFailed           ActionState = "failed"
	ActionAmbiguous        ActionState = "ambiguous"
	ActionStale            ActionState = "stale"
	ActionDenied           ActionState = "denied"
	ActionExpired          ActionState = "expired"
	ActionCanceled         ActionState = "canceled"
)

var (
	// ErrActiveAction means the target already has an action in flight.
	ErrActiveAction     = errors.New("the target has an active action")
	ErrDigestMismatch   = errors.New("the approval names another digest")
	ErrExpired          = errors.New("the approval window has passed")
	ErrAlreadyDecided   = errors.New("the action is no longer awaiting approval")
	ErrNotAuthorized    = errors.New("the action is not authorized")
	ErrApprovalUnusable = errors.New("the approval is consumed or bound to another digest")
)

// Action is a governed action, derived from live state, never from proposal
// text.
type Action struct {
	ID                string
	IncidentID        string
	ProposalID        string
	Capability        string
	CapabilityVersion string
	Target            digest.TargetIdentity
	Params            map[string]any
	PreState          json.RawMessage
	Desired           json.RawMessage
	BlastRadius       map[string]any
	Risk              string
	Decision          string
	DecisionReason    string
	PolicyID          string
	PolicyDigest      string
	Constraints       map[string]any
	Digest            string
	State             ActionState
	ExpiresAt         *time.Time
	CreatedAt         time.Time
}

// Fields returns the digest fields of a.
func (a Action) Fields() digest.ActionFields {
	return digest.ActionFields{
		IncidentID: a.IncidentID, Capability: a.Capability, CapabilityVersion: a.CapabilityVersion,
		Target: a.Target, Params: a.Params, PolicyID: a.PolicyID, PolicyDigest: a.PolicyDigest,
		Constraints: a.Constraints,
	}
}

// InsertAction stores a and returns its ID, or ErrActiveAction when its
// target has an action in flight.
func (s *Store) InsertAction(ctx context.Context, tx pgx.Tx, a Action) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("failed to create action ID: %w", err)
	}
	encoded := make([][]byte, 0, 3)
	for _, v := range []any{a.Params, a.BlastRadius, a.Constraints} {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		encoded = append(encoded, b)
	}
	t := a.Target
	tag, err := tx.Exec(ctx, `
	INSERT INTO actions (id, incident_id, proposal_id, capability, capability_version,
	                     target_group, target_kind, target_namespace, target_name, target_uid,
	                     target_generation, target_spec_hash, target_manager,
	                     params, pre_state, desired_state, blast_radius, risk,
	                     decision, decision_reason, policy_id, policy_digest, constraints,
	                     digest, state, expires_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)
	ON CONFLICT (target_uid) WHERE state IN ('awaiting_approval', 'authorized', 'executing') DO NOTHING
`, id, a.IncidentID, a.ProposalID, a.Capability, a.CapabilityVersion,
		t.Group, t.Kind, t.Namespace, t.Name, t.UID, t.Generation, t.SpecHash, t.Manager,
		encoded[0], jsonOr(a.PreState, "{}"), jsonOr(a.Desired, "{}"), encoded[1], a.Risk,
		a.Decision, a.DecisionReason, a.PolicyID, a.PolicyDigest, encoded[2],
		a.Digest, a.State, a.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("insert action: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrActiveAction
	}
	return id.String(), nil
}

const actionColumns = `id::text, incident_id::text, proposal_id::text, capability, capability_version,
	target_group, target_kind, target_namespace, target_name, target_uid,
	target_generation, target_spec_hash, target_manager,
	params, pre_state, desired_state, blast_radius, risk,
	decision, decision_reason, policy_id, policy_digest, constraints,
	digest, state, expires_at, created_at`

func scanAction(row pgx.Row) (Action, error) {
	var (
		a                                Action
		params, blastRadius, constraints []byte
		preState, desired                []byte
	)
	t := &a.Target
	err := row.Scan(&a.ID, &a.IncidentID, &a.ProposalID, &a.Capability, &a.CapabilityVersion,
		&t.Group, &t.Kind, &t.Namespace, &t.Name, &t.UID, &t.Generation, &t.SpecHash, &t.Manager,
		&params, &preState, &desired, &blastRadius, &a.Risk,
		&a.Decision, &a.DecisionReason, &a.PolicyID, &a.PolicyDigest, &constraints,
		&a.Digest, &a.State, &a.ExpiresAt, &a.CreatedAt)
	if err != nil {
		return Action{}, err
	}
	a.PreState, a.Desired = preState, desired
	for _, f := range []struct {
		data []byte
		into *map[string]any
	}{{params, &a.Params}, {blastRadius, &a.BlastRadius}, {constraints, &a.Constraints}} {
		dec := json.NewDecoder(bytesReader(f.data))
		dec.UseNumber()
		if err := dec.Decode(f.into); err != nil {
			return Action{}, err
		}
		*f.into = normalizeNumbers(*f.into)
	}
	return a, nil
}

// Action returns the action with the given ID; with tx, it is locked.
func (s *Store) Action(ctx context.Context, q Querier, id string) (Action, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Action{}, fmt.Errorf("action %q: %w", id, ErrNotFound)
	}
	sql := `SELECT ` + actionColumns + ` FROM actions WHERE id = $1`
	if _, ok := q.(pgx.Tx); ok {
		sql += ` FOR UPDATE`
	}
	a, err := scanAction(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Action{}, fmt.Errorf("action %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Action{}, fmt.Errorf("read action %s: %w", id, err)
	}
	return a, nil
}

// IncidentActions returns an incident's actions, oldest first.
func (s *Store) IncidentActions(ctx context.Context, q Querier, incidentID string) ([]Action, error) {
	rows, err := q.Query(ctx, `SELECT `+actionColumns+` FROM actions WHERE incident_id = $1 ORDER BY created_at, id`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("actions of incident %s: %w", incidentID, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Action, error) { return scanAction(row) })
}

// SetActionState moves an action from one state to another, or returns
// ErrStateConflict.
func (s *Store) SetActionState(ctx context.Context, tx pgx.Tx, id string, from, to ActionState) error {
	tag, err := tx.Exec(ctx, `UPDATE actions SET state = $3, updated_at = now() WHERE id = $1 AND state = $2`, id, from, to)
	if err != nil {
		return fmt.Errorf("action %s %s->%s: %w", id, from, to, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %s %s->%s: %w", id, from, to, ErrStateConflict)
	}
	return nil
}

// Approve records an approval of the digest the approver saw, and authorizes
// the action. It never approves a digest other than the one given.
func (s *Store) Approve(ctx context.Context, tx pgx.Tx, actionID, dg, approver string, now time.Time) (Action, error) {
	a, err := s.Action(ctx, tx, actionID)
	if err != nil {
		return Action{}, err
	}
	switch {
	case a.State != ActionAwaitingApproval:
		return Action{}, ErrAlreadyDecided
	case a.ExpiresAt != nil && !now.Before(*a.ExpiresAt):
		return Action{}, ErrExpired
	case dg != a.Digest:
		return Action{}, ErrDigestMismatch
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Action{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO approvals (id, action_id, digest, approver, approved_at) VALUES ($1, $2, $3, $4, $5)`,
		id, actionID, dg, approver, now); err != nil {
		return Action{}, fmt.Errorf("record approval of %s: %w", actionID, err)
	}
	if err := s.SetActionState(ctx, tx, actionID, ActionAwaitingApproval, ActionAuthorized); err != nil {
		return Action{}, err
	}
	a.State = ActionAuthorized
	return a, nil
}

// ExpireApprovals moves actions whose approval window passed to expired and
// returns them.
func (s *Store) ExpireApprovals(ctx context.Context, tx pgx.Tx, now time.Time) ([]Action, error) {
	rows, err := tx.Query(ctx, `
	UPDATE actions SET state = 'expired', updated_at = now()
	WHERE state = 'awaiting_approval' AND expires_at <= $1
	RETURNING `+actionColumns, now)
	if err != nil {
		return nil, fmt.Errorf("expire approvals: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Action, error) { return scanAction(row) })
}

// ClaimExecution moves an authorized action to executing and consumes its
// approval, which must be unconsumed and bound to the action's digest. The
// state change is the single-use guard.
func (s *Store) ClaimExecution(ctx context.Context, tx pgx.Tx, a Action, now time.Time) error {
	if a.Decision == "require_approval" {
		tag, err := tx.Exec(ctx, `
	UPDATE approvals SET consumed_at = $3
	WHERE action_id = $1 AND digest = $2 AND consumed_at IS NULL
`, a.ID, a.Digest, now)
		if err != nil {
			return fmt.Errorf("consume approval of %s: %w", a.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrApprovalUnusable
		}
	}
	err := s.SetActionState(ctx, tx, a.ID, ActionAuthorized, ActionExecuting)
	if errors.Is(err, ErrStateConflict) {
		return ErrNotAuthorized
	}
	return err
}

// Execution is the record of one execution of an action.
type Execution struct {
	ActionID       string
	Status         string
	Message        string
	PostGeneration int64
	StartedAt      time.Time
	FinishedAt     time.Time
}

func (s *Store) RecordExecution(ctx context.Context, tx pgx.Tx, e Execution) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
	INSERT INTO executions (id, action_id, status, message, post_generation, started_at, finished_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
`, id, e.ActionID, e.Status, e.Message, e.PostGeneration, e.StartedAt, e.FinishedAt)
	if err != nil {
		return fmt.Errorf("record execution of %s: %w", e.ActionID, err)
	}
	return nil
}

// ActionExecution returns an action's execution record.
func (s *Store) ActionExecution(ctx context.Context, q Querier, actionID string) (Execution, error) {
	var e Execution
	err := q.QueryRow(ctx, `
	SELECT action_id::text, status, message, post_generation, started_at, finished_at
	FROM executions WHERE action_id = $1
`, actionID).Scan(&e.ActionID, &e.Status, &e.Message, &e.PostGeneration, &e.StartedAt, &e.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Execution{}, fmt.Errorf("execution of %s: %w", actionID, ErrNotFound)
	}
	if err != nil {
		return Execution{}, fmt.Errorf("execution of %s: %w", actionID, err)
	}
	return e, nil
}

// CancelPendingActions cancels an incident's actions that await approval.
func (s *Store) CancelPendingActions(ctx context.Context, tx pgx.Tx, incidentID string) error {
	_, err := tx.Exec(ctx, `UPDATE actions SET state = 'canceled', updated_at = now() WHERE incident_id = $1 AND state = 'awaiting_approval'`, incidentID)
	if err != nil {
		return fmt.Errorf("cancel pending actions of incident %s: %w", incidentID, err)
	}
	return nil
}
