package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
)

// RestoreWorker works restore jobs.
type RestoreWorker struct {
	river.WorkerDefaults[RestoreArgs]

	Governor *Worker
}

func (w *RestoreWorker) Work(ctx context.Context, job *river.Job[RestoreArgs]) error {
	return w.Governor.Restore(ctx, job.Args.ActionID)
}

// Restore puts back what action changed, so people investigate the workload
// as it was before Themis acted (design §6.7). It is a governed action of its
// own: a rollback to the revision the fix replaced, decided by policy. When
// policy forbids it or asks a person, Themis leaves its change in place and
// escalates. It acts only on an incident in restoring, so a repeated job does
// nothing.
func (w *Worker) Restore(ctx context.Context, actionID string) error {
	fix, err := w.Store.Action(ctx, w.Store.DB(), actionID)
	if err != nil {
		return err
	}
	in, err := w.Store.Incident(ctx, fix.IncidentID)
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
	var before corev1.PodTemplateSpec
	if err := json.Unmarshal(fix.PreState, &before); err != nil {
		return fmt.Errorf("template before action %s: %w", fix.ID, err)
	}
	revision, err := rollback.RevisionOf(ctx, w.Reader, in.Target, before)
	if errors.Is(err, rollback.ErrNoSuchRevision) {
		return w.escalate(ctx, in.ID, "Themis couldn't put the workload back: Kubernetes no longer keeps the version from before it acted.")
	}
	if errors.Is(err, rollback.ErrStale) {
		return w.escalate(ctx, in.ID, "The workload was deleted or replaced, so there is nothing to put back.")
	}
	if err != nil {
		return err
	}
	live, err := rollback.Read(ctx, w.Reader, in.Target, revision)
	if errors.Is(err, rollback.ErrAlreadyCurrent) {
		return w.escalate(ctx, in.ID, "The workload is already back as it was before Themis acted.")
	}
	if errors.Is(err, rollback.ErrStale) || errors.Is(err, rollback.ErrNoSuchRevision) {
		return w.escalate(ctx, in.ID, "Themis couldn't put the workload back. "+rollback.Explain(err, revision))
	}
	if err != nil {
		return err
	}

	results, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return err
	}
	predicates := map[string]string{}
	for _, r := range results {
		predicates[r.Name] = r.Value.String()
	}
	if live.ManagedBy != "" {
		predicates[predicate.NameGitOpsManaged] = predicate.True.String()
	}
	policy, err := w.policy(profile)
	if err != nil {
		return err
	}
	decision := policy.Evaluate(PolicyInput{
		Capability: rollback.Name, Risk: "medium", Namespace: in.Target.Namespace,
		ManagedBy: live.ManagedBy, Predicates: predicates, Restore: true,
	})
	switch decision.Outcome {
	case Deny:
		return w.escalateDecided(ctx, in.ID, decision, decision.Reason+" Themis left its change in place.")
	case RequireApproval:
		return w.escalateDecided(ctx, in.ID, decision, decision.Reason+" Putting the workload back needs a person, so Themis left its change in place.")
	case Allow:
	}

	a, err := w.restoreAction(in, fix, live, revision, decision)
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
func (w *Worker) restoreAction(in store.Incident, fix store.Action, live rollback.Live, revision int64, d Decision) (store.Action, error) {
	fields := rollback.RestoreFields(in.ID, live, revision, fix.ID, d.PolicyID, d.PolicyDigest)
	dg, err := digest.Compute(fields)
	if err != nil {
		return store.Action{}, err
	}
	pre, err := json.Marshal(live.PreState)
	if err != nil {
		return store.Action{}, err
	}
	desired, err := json.Marshal(live.Desired)
	if err != nil {
		return store.Action{}, err
	}
	return store.Action{
		IncidentID: in.ID, ProposalID: fix.ProposalID,
		Capability: fields.Capability, CapabilityVersion: fields.CapabilityVersion,
		Target: fields.Target, Params: fields.Params, Constraints: fields.Constraints,
		PreState: pre, Desired: desired,
		BlastRadius: map[string]any{"replicas": int64(live.Replicas)},
		Risk:        "medium",
		Decision:    string(Allow), DecisionReason: d.Reason, PolicyID: d.PolicyID, PolicyDigest: d.PolicyDigest,
		Digest: dg, State: store.ActionAuthorized,
	}, nil
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
