// Package executor runs authorized actions. It trusts nothing it is handed
// but an action ID: it recomputes the digest, claims the action once, checks
// that its scope allows the fix, rereads the target and rechecks its identity
// before a dry run and the write.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/verification"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
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

	dryRunFailed bool
}

type Executor struct {
	Store  *store.Store
	Client client.Client
	Logger *slog.Logger
	Now    func() time.Time
	// Fixes are the fixes it can carry out.
	Fixes capability.Registry
	// Scope is what the install lets it change.
	Scope Scope
}

// Scope is what the install lets the executor change, from its own flags,
// whatever the database says was approved.
type Scope struct {
	// Namespaces are those its Roles cover.
	Namespaces []string
	// Fixes are those it may apply, by name. Empty: all it can carry out.
	Fixes []string
}

// Check says what in s the executor can't honour: no namespace, or a fix
// it doesn't have.
func (s Scope) Check(fixes capability.Registry) error {
	var errs []error
	if len(s.Namespaces) == 0 {
		errs = append(errs, errors.New("--namespaces is empty. List the namespaces the executor may change, such as --namespaces=shop"))
	}
	for _, name := range s.Fixes {
		if _, ok := fixes[name]; !ok {
			errs = append(errs, fmt.Errorf("--fixes names %s, which isn't a fix Themis has. Fixes: %s", name, strings.Join(fixes.Names(), ", ")))
		}
	}
	return errors.Join(errs...)
}

// refusal says why s doesn't let the executor apply fix in namespace, or is
// empty when it does.
func (s Scope) refusal(namespace, fix string) string {
	if !slices.Contains(s.Namespaces, namespace) {
		return "Not applied. executor.namespaces doesn't list " + namespace + ", so the executor may not change workloads there."
	}
	if len(s.Fixes) > 0 && !slices.Contains(s.Fixes, fix) {
		return "Not applied. executor.fixes doesn't list " + fix + ", so the executor may not apply it."
	}
	return ""
}

// New returns an executor of fixes within scope, with their metrics started
// at zero. Everything it writes, it writes as workload.FieldManager.
func New(s *store.Store, c client.Client, fixes capability.Registry, scope Scope, log *slog.Logger) *Executor {
	initMetrics(fixes)
	return &Executor{Store: s, Client: client.WithFieldOwner(c, workload.FieldManager), Fixes: fixes, Scope: scope, Logger: log}
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
	// The record, not the caller, says which incident the action is for.
	ctx = telemetry.WithAttrs(ctx, slog.String(telemetry.FieldIncidentID, a.IncidentID))
	if rec, err := e.Store.ActionExecution(ctx, e.Store.DB(), a.ID); err == nil {
		return Result{Status: Status(rec.Status), Message: rec.Message, PostGeneration: rec.PostGeneration}, nil
	}
	started := e.now()
	if a.State == store.ActionExecuting {
		// Claimed, but the executor stopped before recording an outcome.
		res := Result{Status: Ambiguous, Message: "Unknown whether the change was made. The executor stopped while applying it."}
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
		if _, ok := a.Restores(); ok {
			return nil // the incident stays restoring while it is put back
		}
		return e.advance(ctx, tx, a.IncidentID, "Applying the change.", incident.Authorized, incident.Executing)
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
		return Result{Status: Stale, Message: "Not applied. The stored fix was altered after it was approved."}
	}
	if refusal := e.Scope.refusal(a.Target.Namespace, a.Capability); refusal != "" {
		return Result{Status: Failed, Message: refusal}
	}
	f, ok := e.Fixes[a.Capability]
	if !ok {
		return Result{Status: Failed, Message: "Not applied. Themis can't carry out this kind of fix."}
	}

	target := incident.TargetRef{Group: a.Target.Group, Kind: a.Target.Kind, Namespace: a.Target.Namespace, Name: a.Target.Name, UID: a.Target.UID}
	plan, err := f.Plan(ctx, e.Client, target, a.Params)
	if errors.Is(err, capability.ErrInvalidParams) {
		return Result{Status: Failed, Message: "Not applied. Themis can't carry out this kind of fix."}
	}
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		return Result{Status: Stale, Message: "Not applied. " + refusal.Reason}
	}
	if err != nil {
		return Result{Status: Failed, Message: "Not applied. Themis couldn't read the workload: " + err.Error() + "."}
	}
	undone, _ := a.Restores()
	current, err := digest.Compute(capability.Fields(a.IncidentID, f, plan, undone, a.PolicyID, a.PolicyDigest))
	if err != nil || current != a.Digest {
		return Result{Status: Stale, Message: "Not applied. The workload changed after the fix was approved."}
	}

	if _, err := f.Apply(ctx, e.Client, plan, true); err != nil {
		return Result{Status: Failed, Message: "Not applied. Kubernetes rejected the change in a dry run: " + err.Error() + ".", dryRunFailed: true}
	}
	gen, err := f.Apply(ctx, e.Client, plan, false)
	switch {
	case err == nil:
		return Result{Status: Succeeded, Message: plan.Done, PostGeneration: gen}
	case apierrors.IsConflict(err):
		return Result{Status: Stale, Message: "Not applied. The workload changed while the fix was being applied: " + err.Error() + "."}
	case isRejected(err):
		return Result{Status: Failed, Message: "Not applied. Kubernetes rejected the change: " + err.Error() + "."}
	default:
		return Result{Status: Ambiguous, Message: "Unknown whether the change was made. The request failed: " + err.Error() + "."}
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
		store.AfterCommit(tx, actionsTotal.WithLabelValues(e.capabilityLabel(a.Capability), res.label()).Inc)
		_, restore := a.Restores()
		if res.Status != Succeeded {
			e.logger().WarnContext(ctx, "action not executed", telemetry.FieldActionID, a.ID, "status", res.Status, "message", res.Message)
			if restore {
				return e.advance(ctx, tx, a.IncidentID, "Not restored. "+strings.TrimPrefix(res.Message, "Not applied. ")+" Referred to a person.", incident.Restoring, incident.Escalated)
			}
			return e.advance(ctx, tx, a.IncidentID, res.Message+" Referred to a person.", incident.Executing, incident.Escalated)
		}
		if !restore {
			if err := e.advance(ctx, tx, a.IncidentID, res.Message, incident.Executing, incident.Verifying); err != nil {
				return err
			}
		}
		return e.Store.EnqueueTx(ctx, tx, verification.VerifyArgs{ActionID: a.ID, WindowStart: e.now(), IncidentID: a.IncidentID})
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
