package grpcapi

import (
	"context"

	"github.com/hzerrad/themis/internal/catalog/evidence"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func (s *Server) CollectEvidence(ctx context.Context, req *themisv1.CollectEvidenceRequest) (*themisv1.CollectEvidenceResponse, error) {
	got, err := s.Investigations.Collect(ctx, req.GetLeaseId(), req.GetTemplate(), req.GetParams())
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	if got.Failed != nil {
		return &themisv1.CollectEvidenceResponse{
			Failures:          []*themisv1.CollectionFailure{{Target: targetToProto(got.Target), Reason: failureReason(got.Failed), Message: got.Failed.Error()}},
			RemainingRequests: int32Ptr(got.Remaining),
		}, nil
	}
	return &themisv1.CollectEvidenceResponse{
		Items:             []*themisv1.EvidenceItem{itemToProto(got.Item)},
		RemainingRequests: int32Ptr(got.Remaining),
	}, nil
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
