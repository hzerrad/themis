package governance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/workload"
)

// Rechecker reads again, now, the evidence checks read, and decides them
// from it, as catalog.Catalog does.
type Rechecker interface {
	Reread(ctx context.Context, c client.Reader, w *workload.Workload, names []string, window time.Duration, now time.Time, own ...evidence.Item) ([]evidence.Item, []check.Result)
}

// Refusal is why a person can't ask again for approval of a fix, in the
// words people read: "Refused. catalog changed since the fix was planned.
// Investigate again."
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

// Renewable says whether a person can ask again for approval of the
// incident's latest fix, from what Themis keeps of it: Renew reads the
// cluster too, and may still refuse.
func (w *Worker) Renewable(in store.Incident, actions []store.Action) bool {
	return len(actions) > 0 && w.renewable(in, actions, actions[len(actions)-1].ID) == ""
}

// renewable says why a person can't ask again for approval of the action
// id, one of the actions of the incident in, or nothing when they can: it is
// the incident's latest fix, its approval expired with no decision, the
// incident waits for a fix since, and its profiles still offer the fix.
func (w *Worker) renewable(in store.Incident, actions []store.Action, id string) string {
	i := slices.IndexFunc(actions, func(a store.Action) bool { return a.ID == id })
	switch {
	case i < 0 || actions[i].State != store.ActionExpired:
		return "Refused. Only a fix whose approval expired with no decision can be asked for again."
	case i != len(actions)-1:
		return "Refused. A later fix was decided for this incident."
	case in.State != incident.AwaitingSuggestion:
		return "Refused. The incident isn't waiting for a fix."
	}
	f, ok := w.Fixes[actions[i].Capability]
	if !ok {
		return "Refused. Themis can't carry out this kind of fix."
	}
	if why := w.Reach.Why(in.Target.Namespace, f); why != "" {
		return "Refused. " + why
	}
	matched := w.Config.Current().Matched(in.Profiles())
	if len(matched) == 0 {
		return fmt.Sprintf("Refused. Themis no longer has %s this incident matched.", config.Named(in.Profiles()))
	}
	if len(matched.Offering(actions[i].Capability)) == 0 {
		return "Refused. None of the incident's profiles offers this fix."
	}
	return ""
}

// Renew asks again, as the person by, for approval of the fix whose
// approval expired with no decision, so the incident need not be
// investigated again, and the model asked, to reach the same fix. The fix
// must still apply: the workload is exactly as the fix was planned against,
// the incident waits for a fix, and no other fix is under way on the
// workload. The incident's checks are decided again from evidence read now,
// and the fix judged on them exactly as a suggested fix is: one they still
// justify and policy doesn't forbid waits for approval again, with a new
// window, as a new action, and the expired one stays expired. What was read
// joins the incident's evidence, and the checks keep their new answers. A
// *Refusal says why it can't be asked again; nothing is kept then.
func (w *Worker) Renew(ctx context.Context, actionID, by string) (store.Action, error) {
	a, err := w.renew(ctx, actionID, by)
	if _, ok := errors.AsType[*Refusal](err); ok {
		approvalsTotal.WithLabelValues(resultRenewalRefused).Inc()
	}
	return a, err
}

func (w *Worker) renew(ctx context.Context, actionID, by string) (store.Action, error) {
	if w.Recheck == nil {
		return store.Action{}, errors.New("no way to read the evidence again")
	}
	expired, err := w.Store.Action(ctx, w.Store.DB(), actionID)
	if err != nil {
		return store.Action{}, err
	}
	in, err := w.Store.Incident(ctx, expired.IncidentID)
	if err != nil {
		return store.Action{}, err
	}
	actions, err := w.Store.IncidentActions(ctx, w.Store.DB(), in.ID)
	if err != nil {
		return store.Action{}, err
	}
	if why := w.renewable(in, actions, expired.ID); why != "" {
		return store.Action{}, &Refusal{Reason: why}
	}
	p, err := w.Store.Proposal(ctx, w.Store.DB(), expired.ProposalID)
	if err != nil {
		return store.Action{}, err
	}
	target, err := workload.Read(ctx, w.Reader, in.Target)
	if e, ok := errors.AsType[*workload.Error](err); ok {
		return store.Action{}, &Refusal{Reason: fmt.Sprintf("Refused. The workload %s, so the fix no longer applies.", e.Case)}
	}
	if err != nil {
		return store.Action{}, err
	}
	// The same binding an approval has: the workload as the fix was planned
	// against.
	if target.Identity() != expired.Target {
		return store.Action{}, w.changedSince(in.Target.Name)
	}

	stored, err := w.Store.Predicates(ctx, in.ID)
	if err != nil {
		return store.Action{}, err
	}
	read, checks, err := w.reread(ctx, in, target, stored)
	if err != nil {
		return store.Action{}, err
	}
	matched := w.Config.Current().Matched(in.Profiles())
	j, err := w.judge(ctx, in, matched, p, checks, read)
	if err != nil {
		return store.Action{}, err
	}
	noun := w.Fixes.Words(expired.Capability, in.Target.Name, expired.Params, expired.PreState, expired.Desired).Noun
	if why := w.unsupported(j, noun); why != "" {
		return store.Action{}, &Refusal{Reason: why}
	}
	if j.Decided.Action.Target != expired.Target {
		return store.Action{}, w.changedSince(in.Target.Name)
	}

	a := w.awaiting(j.Decided, p, matched.Offering(expired.Capability))
	err = w.Store.InTx(ctx, func(tx pgx.Tx) error {
		// Someone may have moved the incident on meanwhile.
		state, err := w.Store.LockIncident(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		now, err := w.Store.IncidentActions(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		in.State = state
		if why := w.renewable(in, now, expired.ID); why != "" {
			return &Refusal{Reason: why}
		}
		for _, item := range read {
			if err := w.Store.InsertEvidence(ctx, tx, in.ID, "", nil, item); err != nil {
				return err
			}
		}
		for i, r := range j.Checks {
			if !sameResult(r, stored[i]) {
				if err := w.Store.UpdatePredicate(ctx, tx, in.ID, r); err != nil {
					return err
				}
			}
		}
		a.ID, err = w.Store.InsertAction(ctx, tx, a)
		if errors.Is(err, store.ErrActiveAction) {
			return &Refusal{Reason: "Refused. Another fix is already in progress on this workload."}
		}
		if err != nil {
			return err
		}
		store.AfterCommit(tx, approvalsTotal.WithLabelValues(resultRenewed).Inc)
		store.AfterCommit(tx, policyDecisions.WithLabelValues(decisionLabel(j.Decided.Decision)).Inc)
		why := "Approval asked again by " + by + ". The checks still support the " + noun + "."
		return w.Store.TransitionPath(ctx, tx, in.ID, why, by, incident.AwaitingSuggestion, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval)
	})
	if err != nil {
		return store.Action{}, err
	}
	return a, nil
}

// reread reads again, from target as it is now, the evidence the incident's
// checks read, with what Themis's own record holds, such as its fix the
// target runs, and decides them from it, over the windows its investigation
// read. It returns what it read, and the incident's checks with their new
// answers. Whether evidence holds instructions is left to judging, which
// decides it over all the incident's evidence, old and new.
func (w *Worker) reread(ctx context.Context, in store.Incident, target *workload.Workload, stored []check.Result) ([]evidence.Item, []check.Result, error) {
	items, err := w.Store.Evidence(ctx, in.ID)
	if err != nil {
		return nil, nil, err
	}
	var own []evidence.Item
	for _, it := range items {
		if it.Template == evidence.ThemisFix {
			own = append(own, it)
		}
	}
	var names []string
	for _, r := range stored {
		if r.Name != check.EvidenceHasInstructions {
			names = append(names, r.Name)
		}
	}
	read, fresh := w.Recheck.Reread(ctx, w.Reader, target, names, 0, w.now(), own...)
	checks := slices.Clone(stored)
	for _, r := range fresh {
		if i := slices.IndexFunc(checks, func(c check.Result) bool { return c.Name == r.Name }); i >= 0 {
			checks[i] = r
		}
	}
	return read, checks, nil
}

// unsupported says why a fix judged again on checks decided again can't
// wait for approval again, in the words of the refusal, or nothing when it
// can. noun names the fix, such as "raise".
func (w *Worker) unsupported(j Judgement, noun string) string {
	d := j.Decided.Decision
	no := "Refused. The checks no longer support the " + noun + ". "
	switch {
	case j.Refused != "":
		return refusal(j.Refused)
	case d.Outcome != policy.Deny:
		return ""
	case d.PolicyID == UnjustifiedRule:
		return no + strings.TrimPrefix(d.Reason, "Refused. ")
	case len(j.Against) > 0:
		var against []check.Result
		for _, name := range j.Against {
			if i := slices.IndexFunc(j.Checks, func(r check.Result) bool { return r.Name == name }); i >= 0 {
				against = append(against, j.Checks[i])
			}
		}
		return strings.TrimSpace(no + d.Reason + " " + w.Checks.Findings(against))
	}
	return refusal(d.Reason)
}

// refusal is reason as a refusal: "Refused." first.
func refusal(reason string) string {
	if strings.HasPrefix(reason, "Refused.") {
		return reason
	}
	return "Refused. " + reason
}

// changedSince refuses a fix whose workload changed since it was planned,
// and says what a person does instead.
func (w *Worker) changedSince(name string) error {
	next := "Investigate again."
	if w.Config.Current().Investigators.Disabled {
		next = "Suggest another fix."
	}
	return &Refusal{Reason: "Refused. " + name + " changed since the fix was planned. " + next}
}

// sameResult reports whether two answers of a check say the same.
func sameResult(a, b check.Result) bool {
	return a.Value == b.Value && a.Reason == b.Reason && a.Fact == b.Fact && a.Headline == b.Headline && a.Version == b.Version &&
		slices.Equal(a.EvidenceIDs, b.EvidenceIDs)
}
