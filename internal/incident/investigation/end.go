package investigation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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
	couldntDecide = "The investigator couldn't decide."
	suggestedFix  = "The investigator suggested a fix."
	noFixAccepted = "The investigation suggested no fix that Themis could accept."
)

// Complete ends the investigation leaseID holds with r, keeping the gaps the
// investigator reported. The accepted suggestion that goes next is handed to
// governance. Without one, a person takes over an incident none of whose
// profiles offers a fix, told what the checks found. Otherwise an
// inconclusive investigation is handed to a person when the incident's checks
// support a fix the investigator didn't propose; failing that it waits for a
// suggestion if the install gives time for one, and otherwise, like a
// proposed one, closes the incident.
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
		if r.Conclusion == Inconclusive {
			store.CountInvestigation(tx, store.InvestigationAbstained)
			if handed, err := l.Suggestions.HandOver(ctx, tx, inv.IncidentID, inv.LeaseID, couldntDecide, incident.Investigating, incident.Inconclusive); handed || err != nil {
				return err
			}
			if referred, err := l.referDiagnosis(ctx, tx, inv.IncidentID, matched, couldntDecide); referred || err != nil {
				return err
			}
			// Text in the evidence may have talked the investigator out of a
			// fix the checks support: a person decides.
			note, err := l.Suggestions.Unproposed(ctx, inv.IncidentID, matched, inv.Target.Namespace)
			if err != nil {
				return err
			}
			if note != "" {
				return l.Store.TransitionPath(ctx, tx, inv.IncidentID, couldntDecide+" "+note, investigator, incident.Investigating, incident.Escalated)
			}
			// Someone may yet suggest a fix: CloseUndecidedWorker closes it if not.
			if l.Config.Current().SuggestionWindow() > 0 {
				return l.Store.TransitionPath(ctx, tx, inv.IncidentID, couldntDecide, investigator, incident.Investigating, incident.Inconclusive)
			}
			return l.Store.TransitionPath(ctx, tx, inv.IncidentID, couldntDecide, investigator, incident.Investigating, incident.Inconclusive, incident.ClosedNoAction)
		}

		store.CountInvestigation(tx, store.InvestigationProposed)
		if handed, err := l.Suggestions.HandOver(ctx, tx, inv.IncidentID, inv.LeaseID, suggestedFix, incident.Investigating); handed || err != nil {
			return err
		}
		if referred, err := l.referDiagnosis(ctx, tx, inv.IncidentID, matched, noFixAccepted); referred || err != nil {
			return err
		}
		return l.Store.TransitionPath(ctx, tx, inv.IncidentID, noFixAccepted, investigator, incident.Investigating, incident.Diagnosed, incident.ClosedNoAction)
	})
}

// referDiagnosis hands an incident none of whose profiles offers a fix to a
// person, saying after conclusion what the checks found, or that they found
// nothing. Themis can only diagnose such an incident: closing it would drop
// the diagnosis, and nobody can suggest a fix it would take. It reports
// whether it did; it doesn't for an incident whose profiles are all gone.
func (l *Leases) referDiagnosis(ctx context.Context, tx pgx.Tx, incidentID string, profiles config.Matched, conclusion string) (bool, error) {
	if len(profiles) == 0 || len(profiles.Capabilities()) > 0 {
		return false, nil
	}
	results, err := l.Store.Predicates(ctx, incidentID)
	if err != nil {
		return false, err
	}
	names := profiles.Checks()
	var found []string
	for _, r := range results {
		if r.Value != check.True || (names != nil && !slices.Contains(names, r.Name)) {
			continue
		}
		what := r.Name + " is true"
		if c, ok := l.Checks.Get(r.Name); ok && c.Description != "" {
			what = c.Description
		}
		found = append(found, what+": "+strings.TrimSuffix(r.Reason, ".")+".")
	}
	if len(found) == 0 {
		found = []string{"Themis's checks found no cause."}
	}
	reason := conclusion + " " + strings.Join(found, " ") + " Themis has no fix for this kind of incident, so it refers it to a person."
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

// failures say why an investigation failed, as the end of a sentence.
var failures = map[Reason]string{
	Unspecified:        "the investigator gave no reason.",
	ModelUnavailable:   "the model couldn't be reached.",
	InvalidModelOutput: "the model's answers weren't in the required format.",
	BudgetExhausted:    "it used up its token budget.",
	DeadlineExceeded:   "it ran out of time.",
	Internal:           "the investigator hit an internal error.",
}

// budget reports whether r ends an investigation for running out of its
// budget.
func (r Reason) budget() bool { return r == BudgetExhausted || r == DeadlineExceeded }

// failed says why an investigation failed, as a sentence of its own.
func (r Reason) failed() string {
	return "The investigation failed because " + strings.TrimSuffix(failures[r], ".")
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
		return l.abandon(ctx, tx, inv.IncidentID, fmt.Sprintf("The investigation failed %d times. The last time, %s", store.MaxInvestigationAttempts, failures[reason]))
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
// over.
func (l *Leases) abandon(ctx context.Context, tx pgx.Tx, incidentID, reason string) error {
	if _, err := l.Store.LockIncident(ctx, tx, incidentID); err != nil {
		return err
	}
	handed, err := l.Suggestions.HandOver(ctx, tx, incidentID, "", reason+" A fix was suggested from outside, so Themis is checking that.", incident.Investigating)
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	if handed || err != nil {
		return err
	}
	err = l.Store.TransitionPath(ctx, tx, incidentID, reason, investigator, incident.Investigating, incident.Escalated)
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	return err
}
