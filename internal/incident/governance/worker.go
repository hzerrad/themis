package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/store"
)

const actor = "governance"

// Worker works govern jobs: it derives an action from live state, decides it
// by policy, and stores it with the incident's next state.
type Worker struct {
	river.WorkerDefaults[GovernArgs]

	Config config.Provider
	Store  *store.Store
	Reader client.Reader
	Now    func() time.Time
	// Fixes are the fixes Themis can carry out.
	Fixes capability.Registry

	// policies holds compiled policies by digest of their source: profiles
	// can change while Themis runs.
	policies sync.Map
}

// policy returns p's compiled policy.
func (w *Worker) policy(p config.Profile) (*policy.Policy, error) {
	pol := p.EffectivePolicy()
	key, err := json.Marshal(pol)
	if err != nil {
		return nil, err
	}
	if c, ok := w.policies.Load(string(key)); ok {
		return c.(*policy.Policy), nil //nolint:forcetypeassert // only *policy.Policy is stored
	}
	compiled, err := policy.Compile(pol)
	if err != nil {
		return nil, err
	}
	w.policies.Store(string(key), compiled)
	return compiled, nil
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
	plan, err := f.Plan(ctx, w.Reader, in.Target, p.Params)
	if errors.Is(err, capability.ErrInvalidParams) {
		return w.close(ctx, in.ID, "The suggested fix isn't one Themis can carry out.")
	}
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		return w.close(ctx, in.ID, refusal.Reason)
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
	// Live ownership wins over what the evidence said.
	if plan.ManagedBy != "" {
		predicates[check.NameGitOpsManaged] = check.True.String()
	}
	pol, err := w.policy(profile)
	if err != nil {
		return err
	}
	decision := pol.Evaluate(policy.Input{
		Capability: f.Name(), Risk: plan.Risk, Namespace: in.Target.Namespace,
		ManagedBy: plan.ManagedBy, Predicates: predicates,
		Source: p.Provenance["source"], Authenticated: p.Authenticated, External: p.LeaseID == "",
	})

	a, err := w.action(in, p, f, plan, decision, profile.EffectivePolicy())
	if err != nil {
		return err
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		a.ID, err = w.Store.InsertAction(ctx, tx, a)
		if errors.Is(err, store.ErrActiveAction) {
			if err := w.Store.SetProposalStatus(ctx, tx, p.ID, store.ProposalDeferred, "Another fix is already in progress on this workload."); err != nil {
				return err
			}
			return w.advance(ctx, tx, in.ID, "Another fix is already in progress on this workload.", incident.Diagnosed, incident.Recommended, incident.ClosedNoAction)
		}
		if err != nil {
			return err
		}
		store.AfterCommit(tx, policyDecisions.WithLabelValues(decisionLabel(decision)).Inc)
		if decision.Outcome == policy.Deny && plan.ManagedBy != "" {
			store.AfterCommit(tx, gitOpsRefusals.Inc)
		}
		if decision.Outcome == policy.Deny {
			return w.advance(ctx, tx, in.ID, decision.Reason, incident.Diagnosed, incident.Proposed, incident.Recommended, incident.ClosedNoAction)
		}
		return w.advance(ctx, tx, in.ID, decision.Reason, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval)
	})
}

func (w *Worker) action(in store.Incident, p store.Proposal, f capability.Capability, plan capability.Plan, d policy.Decision, pol config.Policy) (store.Action, error) {
	fields := capability.Fields(in.ID, f, plan, "", d.PolicyID, d.PolicyDigest)
	dg, err := digest.Compute(fields)
	if err != nil {
		return store.Action{}, err
	}
	a := store.Action{
		IncidentID: in.ID, ProposalID: p.ID,
		Capability: fields.Capability, CapabilityVersion: fields.CapabilityVersion,
		Target: fields.Target, Params: fields.Params, Constraints: fields.Constraints,
		PreState: plan.Before, Desired: plan.After,
		BlastRadius: plan.BlastRadius,
		Risk:        plan.Risk,
		Decision:    string(d.Outcome), DecisionReason: d.Reason, PolicyID: d.PolicyID, PolicyDigest: d.PolicyDigest,
		Digest: dg, Mark: plan.Mark, State: store.ActionDenied,
	}
	if d.Outcome != policy.Deny {
		expires := w.now().Add(pol.ApprovalTimeout.Duration)
		a.State, a.Decision, a.ExpiresAt = store.ActionAwaitingApproval, string(policy.RequireApproval), &expires
	}
	return a, nil
}

func (w *Worker) close(ctx context.Context, incidentID, reason string) error {
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		return w.advance(ctx, tx, incidentID, reason, incident.Diagnosed, incident.Recommended, incident.ClosedNoAction)
	})
}

func (w *Worker) advance(ctx context.Context, tx pgx.Tx, incidentID, reason string, path ...incident.State) error {
	for i := 1; i < len(path); i++ {
		err := w.Store.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: path[i-1], To: path[i], Reason: reason, Actor: actor})
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
