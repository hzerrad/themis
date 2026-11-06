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
//
// A fix refused while a person works on the incident leaves it waiting for
// another: one a person or an agent suggested, or any once a person denied
// a fix for it or had it investigated again. They can suggest another fix,
// or have it investigated again.
// The investigator's own refused fix hands the incident to a person, who
// suggests another, while its checks find its failure or support a fix, and
// closes it otherwise, as an investigation that proposes nothing does.
// Themis only recommending a fix, GitOps owning the workload, ends it
// either way.
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
	waits, err := w.waits(ctx, p)
	if err != nil {
		return err
	}
	checks, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return err
	}
	// What a person is told when the investigator's fix is refused and the
	// checks still find the failure: they take the incident over.
	referral := ""
	if !waits {
		if referral, err = w.decider.Referral(matched, w.Fixes, checks, w.Checks, in.Target.Namespace); err != nil {
			return err
		}
	}
	f, ok := w.Fixes[p.Capability]
	if !ok {
		return w.refuse(ctx, p, waits, referral, "Refused. Themis can't carry out this kind of fix.")
	}
	redecided, err := w.redecide(ctx, in.ID, checks)
	if err != nil {
		return err
	}
	d, err := w.decider.Decide(ctx, w.Reader, Fix{
		Incident: in.ID, Target: in.Target, Profiles: matched, Checks: checks, Words: w.Checks,
		Capability: f, Params: p.Params,
		By: Suggester{Source: p.Provenance["source"], Authenticated: p.Authenticated, External: p.LeaseID == ""},
	})
	if errors.Is(err, capability.ErrInvalidParams) {
		return w.refuse(ctx, p, waits, referral, "Refused. The suggested fix's values are invalid.")
	}
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		return w.refuse(ctx, p, waits, referral, refusal.Reason)
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
			const busy = "Another fix is already in progress on this workload."
			if err := w.Store.SetProposalStatus(ctx, tx, p.ID, store.ProposalDeferred, busy); err != nil {
				return err
			}
			if waits {
				return w.Store.TransitionPath(ctx, tx, in.ID, busy, actor, incident.Diagnosed, incident.AwaitingSuggestion)
			}
			return w.Store.TransitionPath(ctx, tx, in.ID, busy, actor, incident.Diagnosed, incident.Recommended, incident.ClosedNoAction)
		}
		if err != nil {
			return err
		}
		store.AfterCommit(tx, policyDecisions.WithLabelValues(decisionLabel(d.Decision)).Inc)
		if d.Decision.Outcome == policy.Deny && d.ManagedBy != "" {
			store.AfterCommit(tx, gitOpsRefusals.Inc)
		}
		if d.Decision.Outcome == policy.Deny && waits && d.Decision.PolicyID != GitOpsRule {
			return w.Store.TransitionPath(ctx, tx, in.ID, d.Decision.Reason, actor, incident.Diagnosed, incident.Proposed, incident.AwaitingSuggestion)
		}
		if d.Decision.Outcome == policy.Deny && referral != "" && d.Decision.PolicyID != GitOpsRule {
			return w.Store.ReferPath(ctx, tx, in.ID, referred(d.Decision.Reason, referral), actor, incident.Diagnosed, incident.Proposed, incident.AwaitingSuggestion)
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

// waits reports whether a refusal of p leaves its incident waiting for
// another fix: p came from a person or an agent, or a person works on the
// incident.
func (w *Worker) waits(ctx context.Context, p store.Proposal) (bool, error) {
	if p.LeaseID == "" {
		return true, nil
	}
	return w.Store.PersonInvolved(ctx, w.Store.DB(), p.IncidentID)
}

// refuse ends the governing of p, refused for reason: its incident waits
// for another fix, with p kept as refused so it never goes again. It waits
// handed to a person when referral says what they should know, and closes
// when no one waits for a fix and nothing keeps it open.
func (w *Worker) refuse(ctx context.Context, p store.Proposal, waits bool, referral, reason string) error {
	if !waits && referral == "" {
		return w.close(ctx, p.IncidentID, reason)
	}
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		if err := w.Store.SetProposalStatus(ctx, tx, p.ID, store.ProposalRejected, reason); err != nil {
			return err
		}
		if waits {
			return w.Store.TransitionPath(ctx, tx, p.IncidentID, reason, actor, incident.Diagnosed, incident.AwaitingSuggestion)
		}
		return w.Store.ReferPath(ctx, tx, p.IncidentID, referred(reason, referral), actor, incident.Diagnosed, incident.AwaitingSuggestion)
	})
}

// referred is why a person takes over an incident whose fix was refused for
// reason: the refusal, then what the checks found or support. "Refused. …
// The checks support a memory or CPU change (…). Referred to a person."
func referred(reason, referral string) string {
	return reason + " " + referral + " Referred to a person."
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
