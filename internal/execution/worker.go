// Package execution hands authorized actions to themis-executor.
package execution

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// callTimeout bounds one ExecuteAction call.
const callTimeout = 2 * time.Minute

// Executor is the client side of ExecutorService.
type Executor interface {
	ExecuteAction(ctx context.Context, in *themisv1.ExecuteActionRequest, opts ...grpc.CallOption) (*themisv1.ExecuteActionResponse, error)
}

// Worker works execute jobs. The executor records every outcome itself.
type Worker struct {
	river.WorkerDefaults[governance.ExecuteArgs]

	Store *store.Store

	// Executor is nil when no executor is enabled.
	Executor Executor
}

func (w *Worker) Work(ctx context.Context, job *river.Job[governance.ExecuteArgs]) error {
	return w.Execute(ctx, job.Args.ActionID)
}

func (w *Worker) Execute(ctx context.Context, actionID string) error {
	if w.Executor == nil {
		return w.cancel(ctx, actionID)
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err := w.Executor.ExecuteAction(callCtx, &themisv1.ExecuteActionRequest{ActionId: actionID})
	if c := status.Code(err); c == codes.OK || c == codes.FailedPrecondition || c == codes.NotFound {
		return nil
	}
	// The reply may be lost after the executor acted: its record decides.
	if _, recErr := w.Store.ActionExecution(ctx, w.Store.DB(), actionID); recErr == nil {
		return nil
	} else if !errors.Is(recErr, store.ErrNotFound) {
		return recErr
	}
	return err
}

func (w *Worker) cancel(ctx context.Context, actionID string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		a, err := w.Store.Action(ctx, tx, actionID)
		if err != nil {
			return err
		}
		if err := w.Store.SetActionState(ctx, tx, a.ID, store.ActionAuthorized, store.ActionCanceled); err != nil {
			return nil //nolint:nilerr // already handled
		}
		return w.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: a.IncidentID, From: incident.Authorized, To: incident.Canceled,
			Reason: "no executor is enabled: Themis does not write to the cluster", Actor: "execution",
		})
	})
}
