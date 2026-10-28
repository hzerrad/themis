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

// Timeout outlasts a call, so a slow write is never cut short by River.
func (w *Worker) Timeout(*river.Job[governance.ExecuteArgs]) time.Duration {
	return callTimeout + time.Minute
}

func (w *Worker) Work(ctx context.Context, job *river.Job[governance.ExecuteArgs]) error {
	return w.execute(ctx, job.Args.ActionID, job.Attempt >= job.MaxAttempts)
}

// Execute hands an action to the executor once.
func (w *Worker) Execute(ctx context.Context, actionID string) error {
	return w.execute(ctx, actionID, false)
}

// execute cancels an action that cannot run: an unusable approval, or no
// executor reached by the last attempt. Nothing was written then.
func (w *Worker) execute(ctx context.Context, actionID string, last bool) error {
	if w.Executor == nil {
		return w.cancel(ctx, actionID, "no executor is enabled: Themis does not write to the cluster")
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err := w.Executor.ExecuteAction(callCtx, &themisv1.ExecuteActionRequest{ActionId: actionID})
	c := status.Code(err)
	if c == codes.OK || c == codes.NotFound {
		return nil
	}
	if c == codes.FailedPrecondition {
		// Claimed by an earlier attempt, or refused before anything ran.
		return w.cancel(ctx, actionID, "the executor refused the action: "+status.Convert(err).Message())
	}
	// The reply may be lost after the executor acted: its record decides.
	if _, recErr := w.Store.ActionExecution(ctx, w.Store.DB(), actionID); recErr == nil {
		return nil
	} else if !errors.Is(recErr, store.ErrNotFound) {
		return recErr
	}
	if last {
		return w.cancel(ctx, actionID, "the executor could not be reached: "+err.Error())
	}
	return err
}

// cancel cancels an action still authorized, with its incident; an action
// the executor claimed is left to its record.
func (w *Worker) cancel(ctx context.Context, actionID, reason string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		a, err := w.Store.Action(ctx, tx, actionID)
		if err != nil {
			return err
		}
		if err := w.Store.SetActionState(ctx, tx, a.ID, store.ActionAuthorized, store.ActionCanceled); err != nil {
			return nil //nolint:nilerr // claimed or already handled
		}
		return w.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: a.IncidentID, From: incident.Authorized, To: incident.Canceled, Reason: reason, Actor: "execution",
		})
	})
}
