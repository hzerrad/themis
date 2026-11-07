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
	// Recheck reads an incident's evidence again and decides its checks
	// from it, for a fix whose approval a person asks for again. Without
	// it, nobody can.
	Recheck Rechecker

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
	// What a person is told when the investigator's fix is refused and the
	// checks still find the failure: they take the incident over.
	referral := ""
	if !waits {
		checks, err := w.Store.Predicates(ctx, in.ID)
		if err != nil {
			return err
		}
		if referral, err = w.decider.Referral(matched, w.Fixes, checks, w.Checks, in.Target.Namespace); err != nil {
			return err
		}
	}
	j, err := w.Judge(ctx, in, matched, p)
	if err != nil {
		return err
	}
	if j.Refused != "" {
		return w.refuse(ctx, p, waits, referral, j.Refused)
	}
	d, redecided, f := j.Decided, j.Redecided, w.Fixes[p.Capability]

	// Every fix waits for a person, unless policy forbids it.
	a := d.Action
	a.ProposalID, a.State = p.ID, store.ActionDenied
	if d.Decision.Outcome != policy.Deny {
		a = w.awaiting(d, p, matched.Offering(f.Name()))
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

// Judgement is what governing decides of a suggested fix, before anything is
// kept: why it can't go ahead, or the change worked out against the cluster
// and policy's decision on it.
type Judgement struct {
	// Refused says why the fix can't go ahead before policy decides, such as
	// values the cluster can't take, in the words of the refusal.
	Refused string
	Decided Decided
	// Checks are the incident's checks as the decision read them, and
	// Redecided those decided again for it, which governing keeps.
	Checks    []check.Result
	Redecided []check.Result
	// Against and Past are the checks that bore on the decision, as
	// Decider.Bearing says.
	Against, Past []string
}

// Judge decides p, a fix suggested for in under the profiles matched, as
// governing it decides it, and keeps nothing: Govern keeps what it decides,
// and a preview shows it.
func (w *Worker) Judge(ctx context.Context, in store.Incident, matched config.Matched, p store.Proposal) (Judgement, error) {
	checks, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return Judgement{}, err
	}
	return w.judge(ctx, in, matched, p, checks, nil)
}

// judge decides p on checks, the incident's checks as they stand, with
// more: evidence read for the decision beyond what the incident keeps. It
// keeps nothing.
func (w *Worker) judge(ctx context.Context, in store.Incident, matched config.Matched, p store.Proposal, checks []check.Result, more []evidence.Item) (Judgement, error) {
	j := Judgement{Checks: checks}
	f, ok := w.Fixes[p.Capability]
	if !ok {
		j.Refused = "Refused. Themis can't carry out this kind of fix."
		return j, nil
	}
	items, err := w.Store.Evidence(ctx, in.ID)
	if err != nil {
		return Judgement{}, err
	}
	items = append(items, more...)
	j.Redecided = w.redecide(checks, items)
	flagged := slices.ContainsFunc(items, func(it evidence.Item) bool { return len(it.Suspicious) > 0 })
	fix := Fix{
		Incident: in.ID, Target: in.Target, Profiles: matched, Checks: checks, Words: w.Checks, EvidenceHasInstructions: flagged,
		Capability: f, Params: p.Params,
		By: Suggester{Source: p.Provenance["source"], Authenticated: p.Authenticated, External: p.LeaseID == ""},
	}
	d, err := w.decider.Decide(ctx, w.Reader, fix)
	if errors.Is(err, capability.ErrInvalidParams) {
		j.Refused = "Refused. The suggested fix's values are invalid."
		return j, nil
	}
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		j.Refused = refusal.Reason
		return j, nil
	}
	if err != nil {
		return Judgement{}, err
	}
	j.Decided = d
	if j.Against, j.Past, err = w.decider.Bearing(fix, d); err != nil {
		return Judgement{}, err
	}
	return j, nil
}

// redecide decides check.EvidenceHasInstructions again, when the incident
// runs it, over items: all its evidence, and any read for the decision. The
// investigator asks for more, such as logs, after the checks first run. It
// updates checks in place and returns what changed.
func (w *Worker) redecide(checks []check.Result, items []evidence.Item) []check.Result {
	i := slices.IndexFunc(checks, func(r check.Result) bool { return r.Name == check.EvidenceHasInstructions })
	c, ok := w.Checks.Get(check.EvidenceHasInstructions)
	if i < 0 || !ok {
		return nil
	}
	r := check.List{c}.Evaluate(evidence.Bundle{Items: items}, w.now())[0]
	if r.Value == checks[i].Value && r.Reason == checks[i].Reason {
		return nil
	}
	checks[i] = r
	return []check.Result{r}
}

// awaiting is d's action, for p, waiting for a person's approval: the
// strictest of the profiles governing the fix sets how long they have.
func (w *Worker) awaiting(d Decided, p store.Proposal, governing config.Matched) store.Action {
	a := d.Action
	expires := w.now().Add(governing.ApprovalTimeout())
	a.ProposalID, a.State, a.Decision, a.ExpiresAt = p.ID, store.ActionAwaitingApproval, string(policy.RequireApproval), &expires
	return a
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
