package grpcserver

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func (s *Server) CollectEvidence(ctx context.Context, req *themisv1.CollectEvidenceRequest) (*themisv1.CollectEvidenceResponse, error) {
	var (
		inv       store.Investigation
		remaining int
	)
	// Check before reading the cluster, and again before storing: the lease
	// may expire in between.
	check := func(tx pgx.Tx) error {
		var err error
		inv, err = s.Store.LeasedInvestigation(ctx, tx, req.GetLeaseId(), s.now())
		if err != nil {
			return err
		}
		profile, err := s.profile(inv)
		if err != nil {
			return err
		}
		if !profile.AllowsTemplate(req.GetTemplate()) {
			return status.Errorf(codes.PermissionDenied, "template %q is not allowed for profile %s", req.GetTemplate(), profile.Name)
		}
		used, err := s.Store.CountLeaseEvidence(ctx, tx, inv.LeaseID)
		if err != nil {
			return err
		}
		remaining = profile.InvestigationBudget().MaxEvidenceRequests - used
		if remaining <= 0 {
			return status.Error(codes.ResourceExhausted, "the evidence budget is spent")
		}
		return nil
	}
	if err := s.Store.InTx(ctx, check); err != nil {
		s.refused(ctx, inv, req, err)
		return nil, s.internal(ctx, err)
	}

	item, err := s.Registry.Collect(ctx, s.Reader, req.GetTemplate(), inv.Target, req.GetParams(), s.now())
	if err != nil {
		if errors.Is(err, evidence.ErrInvalidParams) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		s.recordGap(ctx, inv, req, evidence.CauseOf(err), err.Error())
		return &themisv1.CollectEvidenceResponse{
			Failures:          []*themisv1.CollectionFailure{{Target: targetToProto(inv.Target), Reason: failureReason(err), Message: err.Error()}},
			RemainingRequests: int32Ptr(remaining),
		}, nil
	}

	err = s.Store.InTx(ctx, func(tx pgx.Tx) error {
		if err := check(tx); err != nil {
			return err
		}
		return s.Store.InsertEvidence(ctx, tx, inv.IncidentID, inv.LeaseID, req.GetParams(), item)
	})
	if err != nil {
		s.refused(ctx, inv, req, err)
		return nil, s.internal(ctx, err)
	}
	return &themisv1.CollectEvidenceResponse{
		Items:             []*themisv1.EvidenceItem{itemToProto(item)},
		RemainingRequests: int32Ptr(remaining - 1),
	}, nil
}

// refused records a request the profile or the budget turned down. The check
// rolled back with its transaction, so the gap is recorded in another.
func (s *Server) refused(ctx context.Context, inv store.Investigation, req *themisv1.CollectEvidenceRequest, err error) {
	cause, ok := refusals[status.Code(err)]
	// A template Themis does not have is not evidence it could have collected.
	if _, exists := s.Registry[req.GetTemplate()]; !ok || (cause == evidence.GapNotInProfile && !exists) {
		return
	}
	s.recordGap(ctx, inv, req, cause, "")
}

// refusals are the checks that keep evidence from an investigation.
var refusals = map[codes.Code]evidence.GapCause{
	codes.PermissionDenied:  evidence.GapNotInProfile,
	codes.ResourceExhausted: evidence.GapBudgetSpent,
}

// recordGap keeps why the evidence is missing. Failing to keep it does not
// change the investigator's answer, only what the incident can tell later.
func (s *Server) recordGap(ctx context.Context, inv store.Investigation, req *themisv1.CollectEvidenceRequest, cause evidence.GapCause, detail string) {
	if inv.IncidentID == "" {
		return
	}
	g := store.EvidenceGap{
		IncidentID: inv.IncidentID, LeaseID: inv.LeaseID, Template: req.GetTemplate(), Params: req.GetParams(),
		Cause: cause, Detail: detail, At: s.now(),
	}
	if err := s.Store.InTx(ctx, func(tx pgx.Tx) error { return s.Store.InsertEvidenceGap(ctx, tx, g) }); err != nil {
		s.logger().WarnContext(ctx, "cannot record why evidence is missing", "cause", string(cause), telemetry.FieldError, err)
	}
}

// failureReasons tell the investigator why a collection failed.
var failureReasons = map[evidence.GapCause]themisv1.CollectionFailureReason{
	evidence.GapNotFound:  themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_NOT_FOUND,
	evidence.GapForbidden: themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_FORBIDDEN,
	evidence.GapTimeout:   themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_TIMEOUT,
}

func failureReason(err error) themisv1.CollectionFailureReason {
	if r, ok := failureReasons[evidence.CauseOf(err)]; ok {
		return r
	}
	return themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_NOT_AVAILABLE
}

func int32Ptr(n int) *int32 {
	v := int32(max(n, 0)) //nolint:gosec // bounded by the budget
	return &v
}
