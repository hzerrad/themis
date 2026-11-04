package grpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func (s *Server) CompleteInvestigation(ctx context.Context, req *themisv1.CompleteInvestigationRequest) (*themisv1.CompleteInvestigationResponse, error) {
	conclusion, ok := map[themisv1.InvestigationConclusion]string{
		themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_PROPOSED:     "proposed",
		themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_INCONCLUSIVE: "inconclusive",
	}[req.GetConclusion()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "conclusion must be PROPOSED or INCONCLUSIVE")
	}
	result, err := investigationResult(conclusion, req)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "encode result: %v", err)
	}

	err = s.Store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := s.Store.LeasedInvestigation(ctx, tx, req.GetLeaseId(), s.now())
		if err != nil {
			return err
		}
		// Holds off a suggestion from outside until the outcome is decided.
		if _, err := s.Store.LockIncident(ctx, tx, inv.IncidentID); err != nil {
			return err
		}
		reported, err := s.recordReportedGaps(ctx, tx, inv, req.GetGaps())
		if err != nil {
			return err
		}
		if conclusion == "inconclusive" {
			if result.Blind, err = s.blind(ctx, inv.IncidentID, reported); err != nil {
				return err
			}
			kind := "ambiguous"
			if result.Blind {
				kind = "blind"
			}
			store.AfterCommit(tx, abstentionsTotal.WithLabelValues(kind).Inc)
		}
		if err := s.Store.CompleteInvestigation(ctx, tx, inv.LeaseID, result); err != nil {
			return err
		}
		if conclusion == "inconclusive" {
			store.CountInvestigation(tx, store.InvestigationAbstained)
			if handed, err := s.Suggestions.HandOver(ctx, tx, inv.IncidentID, inv.LeaseID, req.GetSummary(), incident.Investigating, incident.Inconclusive); handed || err != nil {
				return err
			}
			// Someone may yet suggest a fix: an expiry job closes it if not.
			if s.Config.Current().ExternalProposals.EffectiveWindow() > 0 {
				return s.Store.TransitionPath(ctx, tx, inv.IncidentID, req.GetSummary(), actor, incident.Investigating, incident.Inconclusive)
			}
			return s.Store.TransitionPath(ctx, tx, inv.IncidentID, req.GetSummary(), actor, incident.Investigating, incident.Inconclusive, incident.ClosedNoAction)
		}

		store.CountInvestigation(tx, store.InvestigationProposed)
		if handed, err := s.Suggestions.HandOver(ctx, tx, inv.IncidentID, inv.LeaseID, req.GetSummary(), incident.Investigating); handed || err != nil {
			return err
		}
		return s.Store.TransitionPath(ctx, tx, inv.IncidentID, "The investigation suggested no fix that Themis could accept.", actor, incident.Investigating, incident.Diagnosed, incident.ClosedNoAction)
	})
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return &themisv1.CompleteInvestigationResponse{}, nil
}

func investigationResult(conclusion string, req *themisv1.CompleteInvestigationRequest) (store.InvestigationResult, error) {
	hypotheses := make([]json.RawMessage, 0, len(req.GetHypotheses()))
	for _, h := range req.GetHypotheses() {
		b, err := protojson.Marshal(h)
		if err != nil {
			return store.InvestigationResult{}, err
		}
		hypotheses = append(hypotheses, b)
	}
	hs, err := json.Marshal(hypotheses)
	if err != nil {
		return store.InvestigationResult{}, err
	}
	usage, err := protojson.Marshal(req.GetUsage())
	if err != nil {
		return store.InvestigationResult{}, err
	}
	provenance, err := protojson.Marshal(req.GetProvenance())
	if err != nil {
		return store.InvestigationResult{}, err
	}
	return store.InvestigationResult{Conclusion: conclusion, Summary: req.GetSummary(), Hypotheses: hs, Usage: usage, Provenance: provenance}, nil
}

func (s *Server) FailInvestigation(ctx context.Context, req *themisv1.FailInvestigationRequest) (*themisv1.FailInvestigationResponse, error) {
	err := s.Store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := s.Store.LeasedInvestigation(ctx, tx, req.GetLeaseId(), s.now())
		if err != nil {
			return err
		}
		if _, err := s.recordReportedGaps(ctx, tx, inv, req.GetGaps()); err != nil {
			return err
		}
		retryAt := s.now().Add(retryBackoff << (inv.Attempts - 1))
		exhausted, err := s.Store.FailInvestigation(ctx, tx, inv.LeaseID, failureKind(req.GetReason()), failed(req.GetReason()), req.GetMessage(), retryAt)
		if err != nil {
			return err
		}
		if budgetReasons[req.GetReason()] {
			store.CountInvestigation(tx, store.InvestigationBudgetExhausted)
		} else {
			store.CountInvestigation(tx, store.InvestigationFailed)
		}
		if !exhausted {
			return nil
		}
		return s.abandon(ctx, tx, inv.IncidentID, fmt.Sprintf("The investigation failed %d times. The last time, %s", store.MaxInvestigationAttempts, failures[req.GetReason()]))
	})
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return &themisv1.FailInvestigationResponse{}, nil
}

// reportedCauses are the gaps only the investigator can see.
var reportedCauses = map[themisv1.EvidenceGapCause]evidence.GapCause{
	themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_NOT_IN_PROFILE: evidence.GapNotInProfile,
	themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_DATA_CEILING:   evidence.GapDataCeiling,
	themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_BUDGET_SPENT:   evidence.GapBudgetSpent,
}

// recordReportedGaps keeps the gaps the investigator met, for templates
// Themis has, and returns their causes.
func (s *Server) recordReportedGaps(ctx context.Context, tx pgx.Tx, inv store.Investigation, gaps []*themisv1.EvidenceGap) ([]evidence.GapCause, error) {
	var causes []evidence.GapCause
	for _, g := range gaps {
		cause, ok := reportedCauses[g.GetCause()]
		if _, exists := s.Evidence[g.GetTemplate()]; !ok || !exists {
			continue
		}
		err := s.Store.InsertEvidenceGap(ctx, tx, store.EvidenceGap{
			IncidentID: inv.IncidentID, LeaseID: inv.LeaseID, Template: g.GetTemplate(),
			Cause: cause, Detail: truncate(g.GetDetail(), maxGapDetail), At: s.now(),
		})
		if err != nil {
			return nil, err
		}
		causes = append(causes, cause)
	}
	return causes, nil
}

// maxGapDetail bounds the detail an investigator reports.
const maxGapDetail = 1024

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// blind reports whether an investigation that couldn't decide was missing
// evidence Themis could have had: a gap it reported, or one recorded before.
func (s *Server) blind(ctx context.Context, incidentID string, reported []evidence.GapCause) (bool, error) {
	for _, c := range reported {
		if c.Blind() {
			return true, nil
		}
	}
	recorded, err := s.Store.EvidenceGaps(ctx, incidentID)
	if err != nil {
		return false, err
	}
	for _, g := range recorded {
		if g.Cause.Blind() {
			return true, nil
		}
	}
	return false, nil
}

// failures say why an investigation failed, as the end of a sentence.
var failures = map[themisv1.InvestigationFailureReason]string{
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_UNSPECIFIED:          "the investigator gave no reason.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE:    "the model couldn't be reached.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_INVALID_MODEL_OUTPUT: "the model's answers weren't in the required format.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_BUDGET_EXHAUSTED:     "it used up its token budget.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_DEADLINE_EXCEEDED:    "it ran out of time.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_INTERNAL:             "the investigator hit an internal error.",
}

// failureKind is the reason as stored: the enum's name, lowercase, without
// its prefix, such as model_unavailable.
func failureKind(r themisv1.InvestigationFailureReason) string {
	return strings.ToLower(strings.TrimPrefix(r.String(), "INVESTIGATION_FAILURE_REASON_"))
}

// failed says why an investigation failed, as a sentence of its own.
func failed(r themisv1.InvestigationFailureReason) string {
	why := strings.TrimSuffix(failures[r], ".")
	return "The investigation failed because " + why
}

// abandon ends an investigation that won't finish. A fix suggested from
// outside was accepted and is still worth checking; otherwise a person takes
// over.
func (s *Server) abandon(ctx context.Context, tx pgx.Tx, incidentID, reason string) error {
	if _, err := s.Store.LockIncident(ctx, tx, incidentID); err != nil {
		return err
	}
	handed, err := s.Suggestions.HandOver(ctx, tx, incidentID, "", reason+" A fix was suggested from outside, so Themis is checking that.", incident.Investigating)
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	if handed || err != nil {
		return err
	}
	return s.escalate(ctx, tx, incidentID, reason)
}

func (s *Server) escalate(ctx context.Context, tx pgx.Tx, incidentID, reason string) error {
	err := s.Store.TransitionPath(ctx, tx, incidentID, reason, actor, incident.Investigating, incident.Escalated)
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	return err
}
