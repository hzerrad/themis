package investigation

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// Close closes an incident waiting for a suggested fix, or for a person to
// make the change Themis recommends, with no change, as the person by asked,
// keeping their reason, if any, exactly as written: "Closed by alice.
// Reason: handled by the vendor." Nothing else closes one whose checks still
// find its failure. A *Refusal says why it can't be.
func Close(ctx context.Context, s *store.Store, incidentID, by, reason string) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		state, err := s.LockIncident(ctx, tx, incidentID)
		if err != nil {
			return err
		}
		if state != incident.AwaitingSuggestion && state != incident.Inconclusive && state != incident.Recommended {
			return &Refusal{Reason: "The incident isn't waiting for a fix."}
		}
		err = s.Transition(ctx, tx, store.TransitionArgs{
			ID: incidentID, From: state, To: incident.ClosedNoAction, Reason: closedBy(by, reason), Actor: by,
		})
		if err != nil {
			return err
		}
		return s.CancelPendingActions(ctx, tx, incidentID)
	})
}

// closedBy says who closed an incident and why, their reason as they wrote
// it.
func closedBy(by, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "Closed by " + by + "."
	}
	// A reason that stops mid-sentence gets the full stop the sentence needs.
	if last, _ := utf8.DecodeLastRuneInString(reason); unicode.IsLetter(last) || unicode.IsDigit(last) {
		reason += "."
	}
	return "Closed by " + by + ". Reason: " + reason
}
