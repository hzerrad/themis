// Package executor runs authorized actions. It trusts nothing it is handed
// but an action ID: it recomputes the digest, claims the action once, rereads
// the target and rechecks its identity before a dry run and the write.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/verify"
)

const actor = "executor"

// executeTimeout bounds an execution, apart from the caller's deadline: a
// caller that stops waiting must not cut a write short.
const executeTimeout = 90 * time.Second

type Status string

const (
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	Ambiguous Status = "ambiguous"
	Stale     Status = "stale"
)

type Result struct {
	Status         Status
	Message        string
	PostGeneration int64
}

type Executor struct {
	Store  *store.Store
	Client client.Client
	Logger *slog.Logger
	Now    func() time.Time
}

// Execute runs an action once. The execution record is committed before it
// returns, so a lost reply can be recovered from the database.
func (e *Executor) Execute(ctx context.Context, actionID string) (Result, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), executeTimeout)
	defer cancel()
	a, err := e.Store.Action(ctx, e.Store.DB(), actionID)
	if err != nil {
		return Result{}, err
	}
	if rec, err := e.Store.ActionExecution(ctx, e.Store.DB(), a.ID); err == nil {
		return Result{Status: Status(rec.Status), Message: rec.Message, PostGeneration: rec.PostGeneration}, nil
	}
	started := e.now()
	if a.State == store.ActionExecuting {
		// Claimed, but the executor stopped before recording an outcome.
		res := Result{Status: Ambiguous, Message: "the execution was interrupted; the write may or may not have happened"}
		return res, e.record(ctx, a, res, started)
	}

	err = e.Store.InTx(ctx, func(tx pgx.Tx) error {
		locked, err := e.Store.Action(ctx, tx, actionID)
		if err != nil {
			return err
		}
		if err := e.Store.ClaimExecution(ctx, tx, locked, started); err != nil {
			return err
		}
		return e.advance(ctx, tx, a.IncidentID, "execution claimed", incident.Authorized, incident.Executing)
	})
	if err != nil {
		return Result{}, err
	}

	res := e.run(ctx, a)
	return res, e.record(ctx, a, res, started)
}

// run never retries a write whose outcome it cannot know.
func (e *Executor) run(ctx context.Context, a store.Action) Result {
	recomputed, err := digest.Compute(a.Fields())
	if err != nil || recomputed != a.Digest {
		return Result{Status: Stale, Message: "the stored action does not match its digest"}
	}
	revision, ok := revisionOf(a.Params)
	if a.Capability != rollback.Name || !ok {
		return Result{Status: Failed, Message: "unknown capability or parameters"}
	}

	target := incident.TargetRef{Group: a.Target.Group, Kind: a.Target.Kind, Namespace: a.Target.Namespace, Name: a.Target.Name, UID: a.Target.UID}
	live, err := rollback.Read(ctx, e.Client, target, revision)
	if err != nil {
		if errors.Is(err, rollback.ErrStale) || errors.Is(err, rollback.ErrNoSuchRevision) || errors.Is(err, rollback.ErrAlreadyCurrent) {
			return Result{Status: Stale, Message: err.Error()}
		}
		return Result{Status: Failed, Message: "read target: " + err.Error()}
	}
	current, err := digest.Compute(rollback.Fields(a.IncidentID, live, revision, a.PolicyID, a.PolicyDigest))
	if err != nil || current != a.Digest {
		return Result{Status: Stale, Message: "the target changed since the action was approved"}
	}

	if _, err := rollback.Apply(ctx, e.Client, live, true); err != nil {
		return Result{Status: Failed, Message: "dry run refused: " + err.Error()}
	}
	gen, err := rollback.Apply(ctx, e.Client, live, false)
	switch {
	case err == nil:
		return Result{Status: Succeeded, Message: fmt.Sprintf("restored revision %d", revision), PostGeneration: gen}
	case apierrors.IsConflict(err):
		return Result{Status: Stale, Message: "the target changed during execution: " + err.Error()}
	case isRejected(err):
		return Result{Status: Failed, Message: err.Error()}
	default:
		return Result{Status: Ambiguous, Message: "the write may or may not have happened: " + err.Error()}
	}
}

// isRejected reports errors the API server answered with: nothing was written.
func isRejected(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err) || apierrors.IsNotFound(err)
}

var actionStates = map[Status]store.ActionState{
	Succeeded: store.ActionSucceeded,
	Failed:    store.ActionFailed,
	Ambiguous: store.ActionAmbiguous,
	Stale:     store.ActionStale,
}

func (e *Executor) record(ctx context.Context, a store.Action, res Result, started time.Time) error {
	// The outcome must be recorded even if the caller gave up waiting.
	ctx = context.WithoutCancel(ctx)
	return e.Store.InTx(ctx, func(tx pgx.Tx) error {
		err := e.Store.RecordExecution(ctx, tx, store.Execution{
			ActionID: a.ID, Status: string(res.Status), Message: res.Message,
			PostGeneration: res.PostGeneration, StartedAt: started, FinishedAt: e.now(),
		})
		if err != nil {
			return err
		}
		if err := e.Store.SetActionState(ctx, tx, a.ID, store.ActionExecuting, actionStates[res.Status]); err != nil {
			return err
		}
		if res.Status != Succeeded {
			e.logger().Warn("action not executed", "action", a.ID, "status", res.Status, "message", res.Message)
			return e.advance(ctx, tx, a.IncidentID, string(res.Status)+": "+res.Message, incident.Executing, incident.Escalated)
		}
		if err := e.advance(ctx, tx, a.IncidentID, res.Message, incident.Executing, incident.Verifying); err != nil {
			return err
		}
		return e.Store.EnqueueTx(ctx, tx, verify.VerifyArgs{ActionID: a.ID, WindowStart: e.now()})
	})
}

func (e *Executor) advance(ctx context.Context, tx pgx.Tx, incidentID, reason string, from, to incident.State) error {
	return e.Store.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: from, To: to, Reason: reason, Actor: actor})
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

func revisionOf(params map[string]any) (int64, bool) {
	n, ok := params["revision"].(int64)
	return n, ok && n > 0
}
