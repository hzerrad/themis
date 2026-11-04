package governance

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/capability"
	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// RestoreWorker works restore jobs.
type RestoreWorker struct {
	river.WorkerDefaults[RestoreArgs]

	Governor *Worker
}

func (w *RestoreWorker) Work(ctx context.Context, job *river.Job[RestoreArgs]) error {
	err := w.Governor.Restore(ctx, job.Args.ActionID)
	if err != nil && job.Attempt >= job.MaxAttempts {
		// The fix is still applied: never leave the incident in restoring.
		return w.Governor.escalateAction(ctx, job.Args.ActionID, "Themis couldn't put the workload back, so its change is still in place: "+err.Error())
	}
	return err
}

// Restore puts back what action changed, so people investigate the workload
// as it was before Themis acted (design §6.7). It is a governed action of its
// own: a rollback to the revision the fix replaced, decided by policy. When
// policy forbids it or asks a person, Themis leaves its change in place and
// escalates. It acts only on an incident in restoring, so a repeated job does
// nothing.
func (w *Worker) Restore(ctx context.Context, actionID string) error {
	applied, err := w.Store.Action(ctx, w.Store.DB(), actionID)
	if err != nil {
		return err
	}
	in, err := w.Store.Incident(ctx, applied.IncidentID)
	if err != nil {
		return err
	}
	if in.State != incident.Restoring {
		return nil
	}
	profile, ok := w.Config.Current().Profile(in.Profile)
	if !ok {
		return w.escalate(ctx, in.ID, fmt.Sprintf("The %s profile was removed from the configuration, so Themis left its change in place.", in.Profile))
	}
	done, ok := w.registry()[applied.Capability]
	if !ok {
		return w.escalate(ctx, in.ID, "Themis doesn't know how to put this fix back, so it left its change in place.")
	}
	name, params, err := done.Undo(ctx, w.Reader, in.Target, applied.PreState)
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		return w.escalate(ctx, in.ID, refusal.Reason)
	}
	if err != nil {
		return err
	}
	undo, ok := w.registry()[name]
	if !ok {
		return w.escalate(ctx, in.ID, "Themis doesn't know how to put this fix back, so it left its change in place.")
	}
	plan, err := undo.Plan(ctx, w.Reader, in.Target, params)
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		if refusal.NoOp {
			return w.escalate(ctx, in.ID, "The workload is already back as it was before Themis acted.")
		}
		return w.escalate(ctx, in.ID, "Themis couldn't put the workload back. "+refusal.Reason)
	}
	if err != nil {
		return err
	}

	// Anything else is someone's later change, not Themis's to undo. From here
	// the digest binds this state, so the executor refuses any later one.
	live, err := done.Mark(ctx, w.Reader, in.Target)
	if err != nil {
		return err
	}
	if live != applied.Mark {
		return w.escalate(ctx, in.ID, "The workload changed after Themis's fix, so Themis left it as it is. A person needs to take over.")
	}

	results, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return err
	}
	predicates := map[string]string{}
	for _, r := range results {
		predicates[r.Name] = r.Value.String()
	}
	if plan.ManagedBy != "" {
		predicates[check.NameGitOpsManaged] = check.True.String()
	}
	policy, err := w.policy(profile)
	if err != nil {
		return err
	}
	decision := policy.Evaluate(PolicyInput{
		Capability: undo.Name(), Risk: plan.Risk, Namespace: in.Target.Namespace,
		ManagedBy: plan.ManagedBy, Predicates: predicates, Restore: true,
	})
	switch decision.Outcome {
	case Deny:
		return w.escalateDecided(ctx, in.ID, decision, decision.Reason+" Themis left its change in place.")
	case RequireApproval:
		return w.escalateDecided(ctx, in.ID, decision, decision.Reason+" Putting the workload back needs a person, so Themis left its change in place.")
	case Allow:
	}

	a, err := w.restoreAction(in, applied, undo, plan, decision)
	if err != nil {
		return err
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		a.ID, err = w.Store.InsertAction(ctx, tx, a)
		if errors.Is(err, store.ErrActiveAction) {
			return w.escalateTx(ctx, tx, in.ID, "Another fix is already in progress on this workload, so Themis left its change in place.")
		}
		if err != nil {
			return err
		}
		store.AfterCommit(tx, policyDecisions.WithLabelValues(decision.label()).Inc)
		return w.Store.EnqueueTx(ctx, tx, ExecuteArgs{ActionID: a.ID, IncidentID: in.ID})
	})
}

// restoreAction is authorized by policy alone: it needs no approval.
func (w *Worker) restoreAction(in store.Incident, applied store.Action, undo capability.Capability, plan capability.Plan, d Decision) (store.Action, error) {
	fields := capability.Fields(in.ID, undo, plan, applied.ID, d.PolicyID, d.PolicyDigest)
	dg, err := digest.Compute(fields)
	if err != nil {
		return store.Action{}, err
	}
	return store.Action{
		IncidentID: in.ID, ProposalID: applied.ProposalID,
		Capability: fields.Capability, CapabilityVersion: fields.CapabilityVersion,
		Target: fields.Target, Params: fields.Params, Constraints: fields.Constraints,
		PreState: plan.Before, Desired: plan.After,
		BlastRadius: plan.BlastRadius,
		Risk:        plan.Risk,
		Decision:    string(Allow), DecisionReason: d.Reason, PolicyID: d.PolicyID, PolicyDigest: d.PolicyDigest,
		Digest: dg, Mark: plan.Mark, State: store.ActionAuthorized,
	}, nil
}

func (w *Worker) escalateAction(ctx context.Context, actionID, reason string) error {
	applied, err := w.Store.Action(ctx, w.Store.DB(), actionID)
	if err != nil {
		return err
	}
	return w.escalate(ctx, applied.IncidentID, reason)
}

func (w *Worker) escalateDecided(ctx context.Context, incidentID string, d Decision, reason string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		store.AfterCommit(tx, policyDecisions.WithLabelValues(d.label()).Inc)
		return w.escalateTx(ctx, tx, incidentID, reason)
	})
}

func (w *Worker) escalate(ctx context.Context, incidentID, reason string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error { return w.escalateTx(ctx, tx, incidentID, reason) })
}

func (w *Worker) escalateTx(ctx context.Context, tx pgx.Tx, incidentID, reason string) error {
	err := w.Store.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: incident.Restoring, To: incident.Escalated, Reason: reason, Actor: actor})
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	return err
}
