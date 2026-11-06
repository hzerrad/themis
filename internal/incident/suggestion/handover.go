package suggestion

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/store"
)

// HandOver hands the accepted suggestion that goes next to governance, moving
// the incident from path through to diagnosed with reason. The one made under
// lease goes first, then the first from outside Themis; an empty lease takes
// only those from outside. One governed already, such as a fix a person
// denied, never goes again. It reports whether there was one.
func (b *Inbox) HandOver(ctx context.Context, tx pgx.Tx, incidentID, lease, reason string, path ...incident.State) (bool, error) {
	proposals, err := b.Store.Proposals(ctx, tx, incidentID)
	if err != nil {
		return false, err
	}
	actions, err := b.Store.IncidentActions(ctx, tx, incidentID)
	if err != nil {
		return false, err
	}
	governed := make(map[string]bool, len(actions))
	for _, a := range actions {
		governed[a.ProposalID] = true
	}
	p, ok := next(proposals, lease, governed)
	if !ok {
		return false, nil
	}
	return true, b.handOver(ctx, tx, incidentID, p.ID, reason, path...)
}

func (b *Inbox) handOver(ctx context.Context, tx pgx.Tx, incidentID, proposalID, reason string, path ...incident.State) error {
	if err := b.Store.TransitionPath(ctx, tx, incidentID, reason, actor, append(path, incident.Diagnosed)...); err != nil {
		return err
	}
	return b.Store.EnqueueTx(ctx, tx, governance.GovernArgs{ProposalID: proposalID, IncidentID: incidentID})
}

// Unproposed says which fixes the incident's checks support, among those its
// profiles offer, that no policy of theirs forbids whatever they change, for
// an investigation that suggested none. It is empty when there is none.
func (b *Inbox) Unproposed(ctx context.Context, incidentID string, profiles config.Matched, namespace string) (string, error) {
	checks, err := b.Store.Predicates(ctx, incidentID)
	if err != nil {
		return "", err
	}
	return b.decider.Unproposed(profiles, b.Fixes, checks, namespace)
}

// next picks the accepted suggestion to govern, among those not governed
// yet: the lease's own first, then the first suggested from outside.
func next(proposals []store.Proposal, lease string, governed map[string]bool) (store.Proposal, bool) {
	for _, own := range []bool{true, false} {
		for _, p := range proposals {
			if p.Status != store.ProposalAccepted || governed[p.ID] {
				continue
			}
			if (own && p.LeaseID == lease) || (!own && p.LeaseID == "") {
				return p, true
			}
		}
	}
	return store.Proposal{}, false
}
