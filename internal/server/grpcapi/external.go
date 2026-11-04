package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

const (
	// Audience is who a caller's token must be issued for, so a token meant
	// for another service can't be replayed here.
	Audience = "themis"

	// PersonSource names a person using the admin token. It is one shared
	// credential, so it proves the token, not which person.
	PersonSource = "admin-token"

	// SignedInSource, followed by a username, names someone signed in.
	SignedInSource = "person:"

	personPerMinute = 60

	// PeerAttemptsPerMinute bounds, per address, calls with a token not
	// recently accepted, before Themis asks the API server about it. A caller
	// with a valid token needs about one a minute.
	PeerAttemptsPerMinute = 30
	// ReadsPerMinute bounds each source's ListIncidents and GetIncident calls.
	ReadsPerMinute = 120

	// maxExternalProposals bounds what one source can pile on an incident.
	maxExternalProposals = 20
	maxExternalText      = 4 << 10
	maxExternalCitations = 50
	maxOpenIncidents     = 100
)

// SubmitFromPerson takes a suggestion from a person using the API: someone
// signed in, or the holder of the admin token when person is empty.
func (s *Server) SubmitFromPerson(ctx context.Context, person string, req *themisv1.SubmitProposalRequest) (*themisv1.SubmitProposalResponse, error) {
	source := PersonSource
	if person != "" {
		source = SignedInSource + person
	}
	return s.submitExternal(ctx, Caller{Source: source, PerMinute: personPerMinute, All: true}, req)
}

// submitExternal stores a suggestion from outside and, if the incident is
// waiting for one, sends it on to governance. It passes the same checks as
// the investigator's. What is refused is kept, with why. An incident outside
// the caller's scope looks missing, so it learns nothing and leaves nothing.
func (s *Server) submitExternal(ctx context.Context, caller Caller, req *themisv1.SubmitProposalRequest) (*themisv1.SubmitProposalResponse, error) {
	if req.GetLeaseId() != "" {
		return nil, status.Error(codes.PermissionDenied, "a lease belongs to the investigator")
	}
	if req.GetIncidentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "incident_id is required")
	}
	if len(req.GetRationale()) > maxExternalText || len(req.GetExpectedEffect()) > maxExternalText ||
		len(req.GetClientProposalId()) > 256 ||
		len(req.GetCitedEvidenceIds()) > maxExternalCitations || len(req.GetCitedPredicates()) > maxExternalCitations {
		return nil, status.Error(codes.InvalidArgument, "the suggestion is too long")
	}
	capability, params, invalid, err := s.parseProposal(req)
	if err != nil {
		return nil, err
	}
	if !s.allow(caller) {
		requestsLimitedTotal.WithLabelValues("suggestions").Inc()
		return nil, status.Error(codes.ResourceExhausted, "too many suggestions: wait a minute and try again")
	}
	in, err := s.Store.Incident(ctx, req.GetIncidentId())
	if err == nil && !caller.inScope(in.Target.Namespace) {
		s.logger().WarnContext(ctx, "a caller named an incident outside its scope", "source", caller.Source, telemetry.FieldIncidentID, in.ID)
		return nil, status.Error(codes.NotFound, "no such incident")
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "no such incident")
	}
	if err != nil {
		return nil, s.internal(ctx, err)
	}

	prov := provenance(req.GetProvenance())
	prov["claimedSource"] = prov["source"]
	prov["source"] = caller.Source

	var stored store.Proposal
	err = s.Store.InTx(ctx, func(tx pgx.Tx) error {
		state, err := s.Store.LockIncident(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		n, err := s.Store.CountExternalProposals(ctx, tx, in.ID, caller.Source)
		if err != nil {
			return err
		}
		if n >= maxExternalProposals {
			return status.Error(codes.ResourceExhausted, "this source has suggested too many fixes for this incident")
		}
		p := store.Proposal{
			IncidentID: in.ID, ClientProposalID: req.GetClientProposalId(),
			Capability: capability, CapabilityVersion: CapabilityRollbackVersion,
			Target: targetFromProto(req.GetTarget()), Params: params,
			ExpectedEffect: req.GetExpectedEffect(), Rationale: req.GetRationale(),
			CitedEvidenceIDs: req.GetCitedEvidenceIds(), CitedPredicates: req.GetCitedPredicates(),
			Provenance: prov, Authenticated: true, Status: store.ProposalAccepted,
			Notes: notes(req.GetNotes()),
		}
		r, err := s.judgeExternal(ctx, tx, caller, in, state, p, invalid)
		if err != nil {
			return err
		}
		if r != nil {
			p.Status, p.RejectionReason, p.Message = store.ProposalRejected, r.reason.String(), r.message
		}
		var created bool
		stored, created, err = s.Store.InsertProposal(ctx, tx, p)
		if err != nil || !created {
			return err
		}
		countProposal(tx, r)
		store.AfterCommit(tx, externalProposalsTotal.WithLabelValues(externalResult(r)).Inc)
		if r != nil {
			if r.reason != themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH {
				return nil
			}
			return s.Store.RecordSafetyEvent(ctx, tx, store.SafetyEvent{
				IncidentID: in.ID, ProposalID: stored.ID, Kind: store.SafetyTargetMismatch, Detail: r.message,
			})
		}
		// While the investigation runs, it picks this up when it finishes.
		if state != incident.Inconclusive && state != incident.AwaitingSuggestion {
			return nil
		}
		if err := s.Store.TransitionPath(ctx, tx, in.ID, "A fix was suggested by "+caller.Source+".", actor, state, incident.Diagnosed); err != nil {
			return err
		}
		return s.Store.EnqueueTx(ctx, tx, governance.GovernArgs{ProposalID: stored.ID, IncidentID: in.ID})
	})
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return proposalResponse(stored), nil
}

// judgeExternal adds to judge what only a caller from outside can get wrong.
func (s *Server) judgeExternal(ctx context.Context, tx pgx.Tx, c Caller, in store.Incident, state incident.State, p store.Proposal, invalid string) (*rejection, error) {
	if state != incident.Investigating && state != incident.AwaitingSuggestion && state != incident.Inconclusive {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_INCIDENT_CLOSED,
			"This incident is past the point of taking suggestions.",
		}, nil
	}
	if !c.All && !slices.Contains(c.Capabilities, p.Capability) {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED,
			fmt.Sprintf("%s may not suggest this kind of fix (%s).", c.Source, p.Capability),
		}, nil
	}
	profile, ok := s.Config.Current().Profile(in.Profile)
	if !ok {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_INCIDENT_CLOSED,
			fmt.Sprintf("The %s profile is no longer configured.", in.Profile),
		}, nil
	}
	return s.judge(ctx, tx, store.Investigation{IncidentID: in.ID, Target: in.Target}, profile, p, invalid)
}

func externalResult(r *rejection) string {
	if r == nil {
		return "accepted"
	}
	return "rejected"
}

// ListIncidents returns the open incidents the caller may suggest fixes for.
func (s *Server) ListIncidents(ctx context.Context, _ *themisv1.ListIncidentsRequest) (*themisv1.ListIncidentsResponse, error) {
	caller, err := s.reader(ctx)
	if err != nil {
		return nil, err
	}
	incidents, err := s.Store.OpenIncidents(ctx, caller.Namespaces, maxOpenIncidents)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	resp := &themisv1.ListIncidentsResponse{}
	for _, in := range incidents {
		resp.Incidents = append(resp.Incidents, openIncident(in))
	}
	return resp, nil
}

// GetIncident returns what Themis knows about an incident in the caller's
// scope: redacted evidence up to its ceiling, and the checks.
func (s *Server) GetIncident(ctx context.Context, req *themisv1.GetIncidentRequest) (*themisv1.GetIncidentResponse, error) {
	caller, err := s.reader(ctx)
	if err != nil {
		return nil, err
	}
	in, err := s.Store.Incident(ctx, req.GetIncidentId())
	// One answer for both, so a caller can't learn of an incident out of its scope.
	if errors.Is(err, store.ErrNotFound) || (err == nil && !caller.inScope(in.Target.Namespace)) {
		return nil, status.Error(codes.NotFound, "no such incident")
	}
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	items, err := s.Store.Evidence(ctx, in.ID)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	results, err := s.Store.Predicates(ctx, in.ID)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	resp := &themisv1.GetIncidentResponse{Incident: openIncident(in)}
	for _, it := range items {
		if it.Classification == evidence.ClassificationInternal || caller.Ceiling == evidence.ClassificationSensitive {
			resp.Evidence = append(resp.Evidence, itemToProto(it))
		} else {
			resp.Withheld++
		}
	}
	for _, r := range results {
		resp.Predicates = append(resp.Predicates, s.predicateToProto(r))
	}
	if profile, ok := s.Config.Current().Profile(in.Profile); ok {
		var allowed []string
		for _, c := range profile.Capabilities {
			if caller.All || slices.Contains(caller.Capabilities, c) {
				allowed = append(allowed, c)
			}
		}
		resp.Capabilities = s.capabilities(allowed)
	}
	return resp, nil
}
