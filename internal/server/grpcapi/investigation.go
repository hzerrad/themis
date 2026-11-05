package grpcapi

import (
	"context"
	"encoding/json"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// conclusions are the conclusions an investigator may report.
var conclusions = map[themisv1.InvestigationConclusion]string{
	themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_PROPOSED:     investigation.Proposed,
	themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_INCONCLUSIVE: investigation.Inconclusive,
}

func (s *Server) CompleteInvestigation(ctx context.Context, req *themisv1.CompleteInvestigationRequest) (*themisv1.CompleteInvestigationResponse, error) {
	conclusion, ok := conclusions[req.GetConclusion()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "conclusion must be PROPOSED or INCONCLUSIVE")
	}
	result, err := investigationResult(conclusion, req)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "encode result: %v", err)
	}
	if err := s.Investigations.Complete(ctx, req.GetLeaseId(), result, gaps(req.GetGaps())); err != nil {
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
	if err := s.Investigations.Fail(ctx, req.GetLeaseId(), reason(req.GetReason()), req.GetMessage(), gaps(req.GetGaps())); err != nil {
		return nil, s.internal(ctx, err)
	}
	return &themisv1.FailInvestigationResponse{}, nil
}

// reason is why the investigation failed, as stored: the enum's name,
// lowercase, without its prefix, such as model_unavailable.
func reason(r themisv1.InvestigationFailureReason) investigation.Reason {
	return investigation.Reason(strings.ToLower(strings.TrimPrefix(r.String(), "INVESTIGATION_FAILURE_REASON_")))
}

// gapCauses are the causes of the gaps only the investigator can see. Any
// other is no cause Themis knows, so the gap isn't kept.
var gapCauses = map[themisv1.EvidenceGapCause]evidence.GapCause{
	themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_NOT_IN_PROFILE: evidence.GapNotInProfile,
	themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_DATA_CEILING:   evidence.GapDataCeiling,
	themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_BUDGET_SPENT:   evidence.GapBudgetSpent,
}

// gaps are the gaps the investigator reports.
func gaps(reported []*themisv1.EvidenceGap) []catalog.Gap {
	out := make([]catalog.Gap, 0, len(reported))
	for _, g := range reported {
		out = append(out, catalog.Gap{Template: g.GetTemplate(), Cause: gapCauses[g.GetCause()], Detail: g.GetDetail()})
	}
	return out
}
