package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/incident"
)

type ProposalStatus string

const (
	ProposalAccepted   ProposalStatus = "accepted"
	ProposalRejected   ProposalStatus = "rejected"
	ProposalSuperseded ProposalStatus = "superseded"
	ProposalDeferred   ProposalStatus = "deferred"
)

// Proposal is an investigator's suggested remediation. It is immutable: a
// changed recommendation is a new proposal.
type Proposal struct {
	ID                string
	IncidentID        string
	LeaseID           string
	ClientProposalID  string
	Capability        string
	CapabilityVersion string
	Target            incident.TargetRef
	Params            map[string]any
	ExpectedEffect    string
	Rationale         string
	CitedEvidenceIDs  []string
	CitedPredicates   []string
	Provenance        map[string]string

	// Authenticated is set from the caller's verified identity, never from
	// what it claims.
	Authenticated bool

	Status          ProposalStatus
	RejectionReason string
	Message         string
	CreatedAt       time.Time
}

// InsertProposal stores p, unless its lease already submitted the same
// client proposal ID. It returns the stored proposal and whether it is new.
func (s *Store) InsertProposal(ctx context.Context, tx pgx.Tx, p Proposal) (Proposal, bool, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Proposal{}, false, fmt.Errorf("failed to create proposal ID: %w", err)
	}
	params, err := json.Marshal(p.Params)
	if err != nil {
		return Proposal{}, false, err
	}
	provenance, err := json.Marshal(p.Provenance)
	if err != nil {
		return Proposal{}, false, err
	}
	t := p.Target
	conflict := `(lease_id, client_proposal_id)`
	if p.LeaseID == "" {
		conflict = `(incident_id, (provenance ->> 'source'), client_proposal_id) WHERE lease_id IS NULL`
	}
	tag, err := tx.Exec(ctx, `
	INSERT INTO proposals (id, incident_id, lease_id, client_proposal_id, capability, capability_version,
	                       target_group, target_kind, target_namespace, target_name, target_uid,
	                       params, expected_effect, rationale, cited_evidence_ids, cited_predicates,
	                       provenance, authenticated, status, rejection_reason, message)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
	ON CONFLICT `+conflict+` DO NOTHING
`, id, p.IncidentID, nullIfEmpty(p.LeaseID), p.ClientProposalID, p.Capability, p.CapabilityVersion,
		t.Group, t.Kind, t.Namespace, t.Name, t.UID,
		params, p.ExpectedEffect, p.Rationale, nonNil(p.CitedEvidenceIDs), nonNil(p.CitedPredicates),
		provenance, p.Authenticated, p.Status, p.RejectionReason, p.Message)
	if err != nil {
		return Proposal{}, false, fmt.Errorf("insert proposal: %w", err)
	}
	if p.LeaseID == "" {
		stored, err := s.queryProposal(ctx, tx, `WHERE lease_id IS NULL AND incident_id = $1 AND provenance ->> 'source' = $2 AND client_proposal_id = $3`,
			p.IncidentID, p.Provenance["source"], p.ClientProposalID)
		return stored, tag.RowsAffected() == 1, err
	}
	stored, err := s.queryProposal(ctx, tx, `WHERE lease_id = $1 AND client_proposal_id = $2`, p.LeaseID, p.ClientProposalID)
	return stored, tag.RowsAffected() == 1, err
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CountExternalProposals counts what one source suggested for an incident.
func (s *Store) CountExternalProposals(ctx context.Context, tx pgx.Tx, incidentID, source string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM proposals WHERE incident_id = $1 AND lease_id IS NULL AND provenance ->> 'source' = $2`, incidentID, source).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count proposals of incident %s: %w", incidentID, err)
	}
	return n, nil
}

// Proposal returns the proposal with the given ID.
func (s *Store) Proposal(ctx context.Context, q Querier, id string) (Proposal, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Proposal{}, fmt.Errorf("proposal %q: %w", id, ErrNotFound)
	}
	return s.queryProposal(ctx, q, `WHERE id = $1`, id)
}

// Proposals returns an incident's proposals, oldest first.
func (s *Store) Proposals(ctx context.Context, q Querier, incidentID string) ([]Proposal, error) {
	rows, err := q.Query(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE incident_id = $1 ORDER BY created_at, id`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("proposals of incident %s: %w", incidentID, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Proposal, error) { return scanProposal(row) })
}

// SetProposalStatus records a later decision about a proposal.
func (s *Store) SetProposalStatus(ctx context.Context, tx pgx.Tx, id string, status ProposalStatus, message string) error {
	if _, err := tx.Exec(ctx, `UPDATE proposals SET status = $2, message = $3 WHERE id = $1`, id, status, message); err != nil {
		return fmt.Errorf("set status of proposal %s: %w", id, err)
	}
	return nil
}

const proposalColumns = `id::text, incident_id::text, coalesce(lease_id::text, ''), client_proposal_id, capability, capability_version,
	target_group, target_kind, target_namespace, target_name, target_uid,
	params, expected_effect, rationale, cited_evidence_ids, cited_predicates,
	provenance, authenticated, status, rejection_reason, message, created_at`

func (s *Store) queryProposal(ctx context.Context, q Querier, where string, args ...any) (Proposal, error) {
	p, err := scanProposal(q.QueryRow(ctx, `SELECT `+proposalColumns+` FROM proposals `+where, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, fmt.Errorf("proposal: %w", ErrNotFound)
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("read proposal: %w", err)
	}
	return p, nil
}

func scanProposal(row pgx.Row) (Proposal, error) {
	var (
		p                  Proposal
		params, provenance []byte
	)
	t := &p.Target
	err := row.Scan(&p.ID, &p.IncidentID, &p.LeaseID, &p.ClientProposalID, &p.Capability, &p.CapabilityVersion,
		&t.Group, &t.Kind, &t.Namespace, &t.Name, &t.UID,
		&params, &p.ExpectedEffect, &p.Rationale, &p.CitedEvidenceIDs, &p.CitedPredicates,
		&provenance, &p.Authenticated, &p.Status, &p.RejectionReason, &p.Message, &p.CreatedAt)
	if err != nil {
		return Proposal{}, err
	}
	if err := json.Unmarshal(params, &p.Params); err != nil {
		return Proposal{}, err
	}
	if err := json.Unmarshal(provenance, &p.Provenance); err != nil {
		return Proposal{}, err
	}
	return p, nil
}

// SafetyEvent records an attempt to act outside an investigation's scope.
type SafetyEvent struct {
	IncidentID string
	ProposalID string
	Kind       string
	Detail     string
}

const SafetyTargetMismatch = "target_mismatch"

func (s *Store) RecordSafetyEvent(ctx context.Context, tx pgx.Tx, ev SafetyEvent) error {
	var proposal *string
	if ev.ProposalID != "" {
		proposal = &ev.ProposalID
	}
	_, err := tx.Exec(ctx, `INSERT INTO safety_events (incident_id, proposal_id, kind, detail) VALUES ($1, $2, $3, $4)`,
		ev.IncidentID, proposal, ev.Kind, ev.Detail)
	if err != nil {
		return fmt.Errorf("record safety event: %w", err)
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
