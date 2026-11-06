package governance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
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
	// Checks are the checks Themis runs: it decides some again before
	// deciding a fix.
	Checks check.List

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
	matched := w.Config.Current().Matched(in.Profiles())
	if len(matched) == 0 {
		return w.close(ctx, in.ID, fmt.Sprintf("Themis no longer has %s this incident matched.", config.Named(in.Profiles())))
	}
	f, ok := w.Fixes[p.Capability]
	if !ok {
		return w.close(ctx, in.ID, "Refused: Themis can't carry out this kind of fix.")
	}
	checks, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return err
	}
	redecided, err := w.redecide(ctx, in.ID, checks)
	if err != nil {
		return err
	}
	d, err := w.decider.Decide(ctx, w.Reader, Fix{
		Incident: in.ID, Target: in.Target, Profiles: matched, Checks: checks,
		Capability: f, Params: p.Params,
		By: Suggester{Source: p.Provenance["source"], Authenticated: p.Authenticated, External: p.LeaseID == ""},
	})
	if errors.Is(err, capability.ErrInvalidParams) {
		return w.close(ctx, in.ID, "Refused: the suggested fix's values are invalid.")
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
		// The strictest of the profiles governing the fix sets how long a
		// person has to approve it.
		expires := w.now().Add(matched.Offering(f.Name()).ApprovalTimeout())
		a.State, a.Decision, a.ExpiresAt = store.ActionAwaitingApproval, string(policy.RequireApproval), &expires
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		for _, r := range redecided {
			if err := w.Store.UpdatePredicate(ctx, tx, in.ID, r); err != nil {
				return err
			}
		}
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

// redecide decides check.EvidenceHasInstructions again, when the incident
// runs it, over all its evidence: the investigator asks for more, such as
// logs, after the checks first run. It updates checks in place and returns
// what changed.
func (w *Worker) redecide(ctx context.Context, incidentID string, checks []check.Result) ([]check.Result, error) {
	i := slices.IndexFunc(checks, func(r check.Result) bool { return r.Name == check.EvidenceHasInstructions })
	c, ok := w.Checks.Get(check.EvidenceHasInstructions)
	if i < 0 || !ok {
		return nil, nil
	}
	items, err := w.Store.Evidence(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	r := check.List{c}.Evaluate(evidence.Bundle{Items: items}, w.now())[0]
	if r.Value == checks[i].Value && r.Reason == checks[i].Reason {
		return nil, nil
	}
	checks[i] = r
	return []check.Result{r}, nil
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
