package grpcserver

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/store"

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
		return nil, s.internal(ctx, err)
	}

	item, err := s.Registry.Collect(ctx, s.Reader, req.GetTemplate(), inv.Target, req.GetParams(), s.now())
	if err != nil {
		if errors.Is(err, evidence.ErrInvalidParams) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
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
		return nil, s.internal(ctx, err)
	}
	return &themisv1.CollectEvidenceResponse{
		Items:             []*themisv1.EvidenceItem{itemToProto(item)},
		RemainingRequests: int32Ptr(remaining - 1),
	}, nil
}

func failureReason(err error) themisv1.CollectionFailureReason {
	switch {
	case errors.Is(err, evidence.ErrTargetGone):
		return themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_NOT_FOUND
	case apierrors.IsForbidden(err):
		return themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_FORBIDDEN
	case errors.Is(err, context.DeadlineExceeded):
		return themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_TIMEOUT
	default:
		return themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_NOT_AVAILABLE
	}
}

func int32Ptr(n int) *int32 {
	v := int32(max(n, 0)) //nolint:gosec // bounded by the budget
	return &v
}
