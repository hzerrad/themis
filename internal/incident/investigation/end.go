package investigation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// retryBackoff delays the lease after a failure, doubling with each attempt,
// so a brief model outage does not use up every attempt.
const retryBackoff = 30 * time.Second

// An investigation's conclusions.
const (
	// Proposed: the investigator suggested a fix.
	Proposed = "proposed"
	// Inconclusive: the investigator couldn't decide.
	Inconclusive = "inconclusive"
)

// What the timeline says of an investigation's end. The investigator's own
// summary is shown apart, as its note: what operators read of a decision
// comes from what Themis can check.
const (
	noFixProposed = "No fix proposed."
	fixProposed   = "Fix proposed."
	noFixAccepted = "No proposed fix was accepted."
	// checkingSuggested follows when a fix suggested from outside goes next.
	checkingSuggested = " Checking the suggested fix."
	referred          = " Referred to a person."
)

// Complete ends the investigation leaseID holds with r, keeping the gaps the
// investigator reported. The accepted suggestion that goes next is handed to
// governance. Without one, a person takes over an incident none of whose
// profiles offers a fix, told what the checks found. An incident a person
// works on, having denied its fix or had it investigated again, waits for a
// suggested fix: the person decides what comes next. Otherwise, while the
// incident's checks find its failure or support a fix, a person takes it
// over and Themis waits for their suggested fix. Only when they do neither
// does an inconclusive investigation wait for a suggestion if the install
// gives time for one, or, like a proposed one, close the incident.
func (l *Leases) Complete(ctx context.Context, leaseID string, r store.InvestigationResult, reported []catalog.Gap) error {
	return l.Store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := l.Store.LeasedInvestigation(ctx, tx, leaseID, l.now())
		if err != nil {
			return err
		}
		// Holds off a suggestion from outside until the outcome is decided.
		if _, err := l.Store.LockIncident(ctx, tx, inv.IncidentID); err != nil {
			return err
		}
		causes, err := l.recordReported(ctx, tx, inv, reported)
		if err != nil {
			return err
		}
		if r.Conclusion == Inconclusive {
			if r.Blind, err = l.blind(ctx, inv.IncidentID, causes); err != nil {
				return err
			}
			kind := "ambiguous"
			if r.Blind {
				kind = "blind"
			}
			store.AfterCommit(tx, abstentionsTotal.WithLabelValues(kind).Inc)
		}
		if err := l.Store.CompleteInvestigation(ctx, tx, inv.LeaseID, r); err != nil {
			return err
		}
		matched := l.Config.Current().Matched(inv.Profiles)
		involved, err := l.Store.PersonInvolved(ctx, tx, inv.IncidentID)
		if err != nil {
			return err
		}
		if r.Conclusion == Inconclusive {
			store.CountInvestigation(tx, store.InvestigationAbstained)
			if handed, err := l.Suggestions.HandOver(ctx, tx, inv.IncidentID, inv.LeaseID, noFixProposed+checkingSuggested, incident.Investigating, incident.Inconclusive); handed || err != nil {
				return err
			}
			// The checks may still support a fix the person denied: that is
			// no reason to refer it.
			if involved {
				return l.Store.TransitionPath(ctx, tx, inv.IncidentID, noFixProposed, investigator, incident.Investigating, incident.AwaitingSuggestion)
			}
			if done, err := l.referDiagnosis(ctx, tx, inv.IncidentID, matched, noFixProposed); done || err != nil {
				return err
			}
			// Text in the evidence may have talked the investigator out of a
			// fix the checks support, and a failure the checks find doesn't
			// go away for want of a fix: a person decides.
			if done, err := l.refer(ctx, tx, inv, matched, noFixProposed); done || err != nil {
				return err
			}
			// Someone may yet suggest a fix: CloseUndecidedWorker closes it if not.
			if l.Config.Current().SuggestionWindow() > 0 {
				return l.Store.TransitionPath(ctx, tx, inv.IncidentID, noFixProposed, investigator, incident.Investigating, incident.Inconclusive)
			}
			return l.Store.TransitionPath(ctx, tx, inv.IncidentID, noFixProposed, investigator, incident.Investigating, incident.Inconclusive, incident.ClosedNoAction)
		}

		store.CountInvestigation(tx, store.InvestigationProposed)
		if handed, err := l.Suggestions.HandOver(ctx, tx, inv.IncidentID, inv.LeaseID, fixProposed, incident.Investigating); handed || err != nil {
			return err
		}
		if done, err := l.referDiagnosis(ctx, tx, inv.IncidentID, matched, noFixAccepted); done || err != nil {
			return err
		}
		if involved {
			return l.Store.TransitionPath(ctx, tx, inv.IncidentID, noFixAccepted, investigator, incident.Investigating, incident.AwaitingSuggestion)
		}
		if done, err := l.refer(ctx, tx, inv, matched, noFixAccepted); done || err != nil {
			return err
		}
		return l.Store.TransitionPath(ctx, tx, inv.IncidentID, noFixAccepted, investigator, incident.Investigating, incident.Diagnosed, incident.ClosedNoAction)
	})
}

// refer hands an incident whose checks find its failure, or support a fix,
// to a person rather than closing it with no change, saying after
// conclusion which fix they support or what they found: "No fix proposed.
// The checks support a rollback (the failures started after the rollout).
// Referred to a person." It waits for their suggested fix. It reports
// whether it did.
func (l *Leases) refer(ctx context.Context, tx pgx.Tx, inv store.Investigation, profiles config.Matched, conclusion string) (bool, error) {
	note, err := l.Suggestions.Referral(ctx, inv.IncidentID, profiles, inv.Target.Namespace)
	if note == "" || err != nil {
		return false, err
	}
	return true, l.Store.ReferPath(ctx, tx, inv.IncidentID, conclusion+" "+note+referred, investigator, incident.Investigating, incident.AwaitingSuggestion)
}

// referDiagnosis hands an incident none of whose profiles offers a fix to a
// person, saying after conclusion what the checks found, or that they found
// nothing: "No fix proposed. The checks found that an image can't be pulled
// (…). Themis has no fix for this kind of incident. Referred to a person."
// Themis can only diagnose such an incident: closing it would drop the
// diagnosis, and nobody can suggest a fix it would take. It reports whether
// it did; it doesn't for an incident whose profiles are all gone.
func (l *Leases) referDiagnosis(ctx context.Context, tx pgx.Tx, incidentID string, profiles config.Matched, conclusion string) (bool, error) {
	if len(profiles) == 0 || len(profiles.Capabilities()) > 0 {
		return false, nil
	}
	results, err := l.Store.Predicates(ctx, incidentID)
	if err != nil {
		return false, err
	}
	names := profiles.Checks()
	var found []check.Result
	for _, r := range results {
		if r.Value == check.True && (names == nil || slices.Contains(names, r.Name)) {
			found = append(found, r)
		}
	}
	findings := l.Checks.Findings(found)
	if findings == "" {
		findings = "No check found a cause."
	}
	reason := conclusion + " " + findings + " Themis has no fix for this kind of incident." + referred
	return true, l.Store.TransitionPath(ctx, tx, incidentID, reason, investigator, incident.Investigating, incident.Escalated)
}

// Reason is why an investigation failed, as stored, such as
// model_unavailable.
type Reason string

// The reasons an investigation fails.
const (
	Unspecified        Reason = "unspecified"
	ModelUnavailable   Reason = "model_unavailable"
	InvalidModelOutput Reason = "invalid_model_output"
	BudgetExhausted    Reason = "budget_exhausted"
	DeadlineExceeded   Reason = "deadline_exceeded"
	Internal           Reason = "internal"
)

// failures say why an investigation failed, to follow "Last error: ".
var failures = map[Reason]string{
	Unspecified:        "no reason given",
	ModelUnavailable:   "the model was unreachable",
	InvalidModelOutput: "the model's answers didn't follow the reply format",
	BudgetExhausted:    "the token budget ran out",
	DeadlineExceeded:   "the time limit was reached",
	Internal:           "an internal error in the investigator",
}

// stoppedResponding is why an investigation whose last lease expired failed.
const stoppedResponding = "the investigator stopped responding"

// budget reports whether r ends an investigation for running out of its
// budget.
func (r Reason) budget() bool { return r == BudgetExhausted || r == DeadlineExceeded }

// failed says why an investigation failed, as the start of a sentence, such
// as "The model was unreachable", for the investigator's message to follow.
func (r Reason) failed() string {
	why := []rune(r.why())
	why[0] = unicode.ToUpper(why[0])
	return string(why)
}

// why is why r failed an investigation; a reason Themis doesn't know gave
// none.
func (r Reason) why() string {
	if w, ok := failures[r]; ok {
		return w
	}
	return failures[Unspecified]
}

// gaveUp says why an investigation that won't finish was given up:
// "Investigation failed after 3 attempts. Last error: the model was
// unreachable."
func gaveUp(why string) string {
	return fmt.Sprintf("Investigation failed after %d attempts. Last error: %s.", store.MaxInvestigationAttempts, why)
}

// Fail ends the attempt leaseID holds for reason, saying message, and keeps
// the gaps the investigator reported. The investigation is leased again after
// a backoff, unless that was its last attempt: then it is given up.
func (l *Leases) Fail(ctx context.Context, leaseID string, reason Reason, message string, reported []catalog.Gap) error {
	return l.Store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := l.Store.LeasedInvestigation(ctx, tx, leaseID, l.now())
		if err != nil {
			return err
		}
		if _, err := l.recordReported(ctx, tx, inv, reported); err != nil {
			return err
		}
		retryAt := l.now().Add(retryBackoff << (inv.Attempts - 1))
		exhausted, err := l.Store.FailInvestigation(ctx, tx, inv.LeaseID, string(reason), reason.failed(), message, retryAt)
		if err != nil {
			return err
		}
		if reason.budget() {
			store.CountInvestigation(tx, store.InvestigationBudgetExhausted)
		} else {
			store.CountInvestigation(tx, store.InvestigationFailed)
		}
		if !exhausted {
			return nil
		}
		return l.abandon(ctx, tx, inv.IncidentID, gaveUp(reason.why()))
	})
}

// recordReported keeps the gaps the investigator met, and returns the causes
// of those kept.
func (l *Leases) recordReported(ctx context.Context, tx pgx.Tx, inv store.Investigation, gaps []catalog.Gap) ([]evidence.GapCause, error) {
	var causes []evidence.GapCause
	for _, g := range gaps {
		kept, err := recordGap(ctx, l.Store, tx, l.Evidence, store.EvidenceGap{
			IncidentID: inv.IncidentID, LeaseID: inv.LeaseID, Template: g.Template,
			Cause: g.Cause, Detail: g.Detail, At: l.now(),
		})
		if err != nil {
			return nil, err
		}
		if kept {
			causes = append(causes, g.Cause)
		}
	}
	return causes, nil
}

// blind reports whether an investigation that couldn't decide was missing
// evidence Themis could have had: a gap it reported, or one recorded before.
func (l *Leases) blind(ctx context.Context, incidentID string, reported []evidence.GapCause) (bool, error) {
	for _, c := range reported {
		if c.Blind() {
			return true, nil
		}
	}
	recorded, err := l.Store.EvidenceGaps(ctx, incidentID)
	if err != nil {
		return false, err
	}
	for _, g := range recorded {
		if g.Cause.Blind() {
			return true, nil
		}
	}
	return false, nil
}

// abandon ends an investigation that won't finish. A fix suggested from
// outside was accepted and is still worth checking; otherwise a person takes
// over, and Themis waits for the fix they suggest when the incident's
// profiles offer one.
func (l *Leases) abandon(ctx context.Context, tx pgx.Tx, incidentID, reason string) error {
	if _, err := l.Store.LockIncident(ctx, tx, incidentID); err != nil {
		return err
	}
	handed, err := l.Suggestions.HandOver(ctx, tx, incidentID, "", reason+checkingSuggested, incident.Investigating)
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	if handed || err != nil {
		return err
	}
	in, err := l.Store.Incident(ctx, incidentID)
	if err != nil {
		return err
	}
	to := incident.Escalated
	if len(l.Config.Current().Matched(in.Profiles()).Capabilities()) > 0 {
		to = incident.AwaitingSuggestion
	}
	err = l.Store.ReferPath(ctx, tx, incidentID, reason+referred, investigator, incident.Investigating, to)
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	return err
}
