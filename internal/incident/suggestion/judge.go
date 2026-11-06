package suggestion

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// rejection is why a suggestion is stored as rejected.
type rejection struct {
	reason  themisv1.RejectionReason
	message string
}

// subject is the incident a suggestion is for, as judging needs it.
type subject struct {
	incidentID string
	target     incident.TargetRef
	// profiles are the incident's profiles still configured, and names all
	// it matched.
	profiles config.Matched
	names    []string
	// state is the incident's, for a suggestion from outside.
	state incident.State
}

// open are the states in which an incident takes a suggestion from outside.
var open = []incident.State{incident.Investigating, incident.AwaitingSuggestion, incident.Inconclusive}

// judge returns why a suggestion must be rejected, or nil. The target must be
// exactly the incident's; a mismatch is a safety event, never corrected.
// invalid says why its parameters are wrong.
func (b *Inbox) judge(ctx context.Context, tx pgx.Tx, from Suggester, in subject, p store.Proposal, invalid string) (*rejection, error) {
	if from.outside() {
		if !slices.Contains(open, in.state) {
			return &rejection{
				themisv1.RejectionReason_REJECTION_REASON_INCIDENT_CLOSED,
				"Refused: the incident no longer takes suggestions.",
			}, nil
		}
		if !from.All && !slices.Contains(from.Fixes, p.Capability) {
			return &rejection{
				themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED,
				fmt.Sprintf("Refused: %s may not suggest %s.", from.Source, p.Capability),
			}, nil
		}
		if len(in.profiles) == 0 {
			return &rejection{
				themisv1.RejectionReason_REJECTION_REASON_INCIDENT_CLOSED,
				fmt.Sprintf("Refused: Themis no longer has %s this incident matched.", config.Named(in.names)),
			}, nil
		}
	}
	// Only a profile of the incident's that offers the fix lets it through,
	// whoever suggests it.
	if _, known := b.Fixes[p.Capability]; !known || len(in.profiles.Offering(p.Capability)) == 0 {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED,
			notOffered(in.profiles.Names(), p.Capability),
		}, nil
	}
	if p.Target != in.target {
		t := p.Target
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH,
			fmt.Sprintf("Refused: it names %s %s/%s (uid %s); the incident is about %s/%s (uid %s).",
				t.Kind, t.Namespace, t.Name, t.UID, in.target.Namespace, in.target.Name, in.target.UID),
		}, nil
	}
	if invalid != "" {
		return &rejection{themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS, "Refused: " + invalid}, nil
	}
	if len(p.CitedEvidenceIDs) == 0 {
		return &rejection{themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE, "Refused: it cites no evidence."}, nil
	}
	missing, err := b.Store.MissingEvidence(ctx, tx, in.incidentID, p.CitedEvidenceIDs)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE,
			"Refused: it cites evidence Themis didn't collect: " + strings.Join(missing, ", ") + ".",
		}, nil
	}
	if len(p.CitedPredicates) == 0 {
		return nil, nil
	}
	// A check is cited for what it answered for this incident: one Themis
	// didn't run for it answered nothing.
	ran, err := b.Store.Predicates(ctx, in.incidentID)
	if err != nil {
		return nil, err
	}
	for _, name := range p.CitedPredicates {
		if !slices.ContainsFunc(ran, func(r check.Result) bool { return r.Name == name }) {
			return &rejection{themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_PREDICATE, "Refused: it cites a check Themis didn't run for this incident: " + name + "."}, nil
		}
	}
	return nil, nil
}

// notOffered says that none of the profiles named offers fix.
func notOffered(names []string, fix string) string {
	if len(names) == 1 {
		return fmt.Sprintf("Refused: the %s profile doesn't allow %s.", names[0], fix)
	}
	return fmt.Sprintf("Refused: none of the incident's profiles (%s) allows %s.", strings.Join(names, ", "), fix)
}

// sentence makes a fix's error about its parameters the grounds of a
// refusal, to follow "Refused: ".
func sentence(err error) string {
	msg := strings.TrimPrefix(err.Error(), capability.ErrInvalidParams.Error()+": ")
	if msg == "" {
		return "the fix's parameters aren't valid."
	}
	return msg + "."
}
