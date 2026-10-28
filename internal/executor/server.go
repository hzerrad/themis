package executor

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// Server serves ExecutorService. Only the action ID is read from a request.
type Server struct {
	themisv1.UnimplementedExecutorServiceServer

	Executor *Executor
}

func (s *Server) Register(g *grpc.Server) {
	themisv1.RegisterExecutorServiceServer(g, s)
}

var statuses = map[Status]themisv1.ExecuteActionStatus{
	Succeeded: themisv1.ExecuteActionStatus_EXECUTE_ACTION_STATUS_SUCCEEDED,
	Failed:    themisv1.ExecuteActionStatus_EXECUTE_ACTION_STATUS_FAILED,
	Ambiguous: themisv1.ExecuteActionStatus_EXECUTE_ACTION_STATUS_AMBIGUOUS,
	Stale:     themisv1.ExecuteActionStatus_EXECUTE_ACTION_STATUS_STALE,
}

func (s *Server) ExecuteAction(ctx context.Context, req *themisv1.ExecuteActionRequest) (*themisv1.ExecuteActionResponse, error) {
	res, err := s.Executor.Execute(ctx, req.GetActionId())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, status.Error(codes.NotFound, "no such action")
	case errors.Is(err, store.ErrNotAuthorized), errors.Is(err, store.ErrApprovalUnusable):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	case err != nil:
		s.Executor.logger().Error("execution failed", "action", req.GetActionId(), "error", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &themisv1.ExecuteActionResponse{Status: statuses[res.Status], Message: res.Message, PostGeneration: res.PostGeneration}, nil
}
