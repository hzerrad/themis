package governance

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

// ExecuteWorker hands authorized actions to themis-executor: it works
// execute jobs. The executor records every outcome itself.
type ExecuteWorker struct {
	river.WorkerDefaults[ExecuteArgs]

	Store *store.Store

	// Executor is nil when no executor is enabled.
	Executor Executor
}

// Timeout outlasts a call, so a slow write is never cut short by River.
func (w *ExecuteWorker) Timeout(*river.Job[ExecuteArgs]) time.Duration {
	return callTimeout + time.Minute
}

func (w *ExecuteWorker) Work(ctx context.Context, job *river.Job[ExecuteArgs]) error {
	return w.execute(ctx, job.Args.ActionID, job.Attempt >= job.MaxAttempts)
}

// Execute hands an action to the executor once.
func (w *ExecuteWorker) Execute(ctx context.Context, actionID string) error {
	return w.execute(ctx, actionID, false)
}

// execute cancels an action that cannot run: an unusable approval, or no
// executor reached by the last attempt. Nothing was written then.
func (w *ExecuteWorker) execute(ctx context.Context, actionID string, last bool) error {
	if w.Executor == nil {
		return w.cancel(ctx, actionID, "The executor is turned off. Turn it on with executor.enabled=true in the Helm values.")
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
		return w.cancel(ctx, actionID, "The executor refused it: "+status.Convert(err).Message()+".")
	}
	// The reply may be lost after the executor acted: its record decides.
	if _, recErr := w.Store.ActionExecution(ctx, w.Store.DB(), actionID); recErr == nil {
		return nil
	} else if !errors.Is(recErr, store.ErrNotFound) {
		return recErr
	}
	if last {
		return w.cancel(ctx, actionID, "The executor couldn't be reached: "+err.Error()+".")
	}
	return err
}

// cancel cancels an action still authorized, with its incident; an action
// the executor claimed is left to its record. A restore that can't run leaves
// the fix in place, so a person takes over. why is a sentence, to follow
// "Not applied."
func (w *ExecuteWorker) cancel(ctx context.Context, actionID, why string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		a, err := w.Store.Action(ctx, tx, actionID)
		if err != nil {
			return err
		}
		if err := w.Store.SetActionState(ctx, tx, a.ID, store.ActionAuthorized, store.ActionCanceled); err != nil {
			return nil //nolint:nilerr // claimed or already handled
		}
		args := store.TransitionArgs{ID: a.IncidentID, From: incident.Authorized, To: incident.Canceled, Reason: "Not applied. " + why, Actor: "execution"}
		if _, restore := a.Restores(); restore {
			args.From, args.To, args.Reason = incident.Restoring, incident.Escalated, "Not restored. "+why+" The fix stays in place. Referred to a person."
		}
		return w.Store.Transition(ctx, tx, args)
	})
}
