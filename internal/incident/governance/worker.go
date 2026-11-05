package governance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/store"
)

const actor = "governance"

// Worker works govern jobs: it decides an accepted suggestion's fix, and
// stores its action with the incident's next state.
type Worker struct {
	river.WorkerDefaults[GovernArgs]

	Config config.Provider
	Store  *store.Store
	Reader client.Reader
	Now    func() time.Time
	// Fixes are the fixes Themis can carry out.
	Fixes capability.Registry

	decider Decider
}

func (w *Worker) Work(ctx context.Context, job *river.Job[GovernArgs]) error {
	return w.Govern(ctx, job.Args.ProposalID)
}

// Govern acts only on a diagnosed incident, so a repeated job does nothing.
func (w *Worker) Govern(ctx context.Context, proposalID string) error {
	p, err := w.Store.Proposal(ctx, w.Store.DB(), proposalID)
	if err != nil {
		return err
	}
	in, err := w.Store.Incident(ctx, p.IncidentID)
	if err != nil {
		return err
	}
	if in.State != incident.Diagnosed || p.Status != store.ProposalAccepted {
		return nil
	}
	profile, ok := w.Config.Current().Profile(in.Profile)
	if !ok {
		return w.close(ctx, in.ID, fmt.Sprintf("The %s profile was removed from the configuration, so Themis stopped working on this incident.", in.Profile))
	}
	f, ok := w.Fixes[p.Capability]
	if !ok {
		return w.close(ctx, in.ID, "The suggested fix isn't one Themis can carry out.")
	}
	checks, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return err
	}
	d, err := w.decider.Decide(ctx, w.Reader, Fix{
		Incident: in.ID, Target: in.Target, Profile: profile, Checks: checks,
		Capability: f, Params: p.Params,
		By: Suggester{Source: p.Provenance["source"], Authenticated: p.Authenticated, External: p.LeaseID == ""},
	})
	if errors.Is(err, capability.ErrInvalidParams) {
		return w.close(ctx, in.ID, "The suggested fix isn't one Themis can carry out.")
	}
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		return w.close(ctx, in.ID, refusal.Reason)
	}
	if err != nil {
		return err
	}

	// Every fix waits for a person, unless policy forbids it.
	a := d.Action
	a.ProposalID, a.State = p.ID, store.ActionDenied
	if d.Decision.Outcome != policy.Deny {
		expires := w.now().Add(profile.EffectivePolicy().ApprovalTimeout.Duration)
		a.State, a.Decision, a.ExpiresAt = store.ActionAwaitingApproval, string(policy.RequireApproval), &expires
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		a.ID, err = w.Store.InsertAction(ctx, tx, a)
		if errors.Is(err, store.ErrActiveAction) {
			if err := w.Store.SetProposalStatus(ctx, tx, p.ID, store.ProposalDeferred, "Another fix is already in progress on this workload."); err != nil {
				return err
			}
			return w.Store.TransitionPath(ctx, tx, in.ID, "Another fix is already in progress on this workload.", actor, incident.Diagnosed, incident.Recommended, incident.ClosedNoAction)
		}
		if err != nil {
			return err
		}
		store.AfterCommit(tx, policyDecisions.WithLabelValues(decisionLabel(d.Decision)).Inc)
		if d.Decision.Outcome == policy.Deny && d.ManagedBy != "" {
			store.AfterCommit(tx, gitOpsRefusals.Inc)
		}
		if d.Decision.Outcome == policy.Deny {
			return w.Store.TransitionPath(ctx, tx, in.ID, d.Decision.Reason, actor, incident.Diagnosed, incident.Proposed, incident.Recommended, incident.ClosedNoAction)
		}
		return w.Store.TransitionPath(ctx, tx, in.ID, d.Decision.Reason, actor, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval)
	})
}

func (w *Worker) close(ctx context.Context, incidentID, reason string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		return w.Store.TransitionPath(ctx, tx, incidentID, reason, actor, incident.Diagnosed, incident.Recommended, incident.ClosedNoAction)
	})
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
