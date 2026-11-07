package suggestion

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/store"
)

// Governor decides a suggested fix as governing decides it, keeping nothing.
type Governor interface {
	Judge(ctx context.Context, in store.Incident, matched config.Matched, p store.Proposal) (governance.Judgement, error)
}

// Preview is what Themis would do with a suggestion: judged as Submit judges
// it, then decided as governing decides it, with nothing kept.
type Preview struct {
	Incident store.Incident
	// Matched are the incident's profiles still configured.
	Matched config.Matched
	// Proposal is the suggestion as it would be kept, its parameters as
	// its fix keeps them.
	Proposal store.Proposal
	// Refused says why Themis would refuse it, in the words of the refusal
	// it would keep; empty when the fix would go to a person.
	Refused string
	// Judgement is governing's, once the suggestion gets that far.
	Judgement *governance.Judgement
}

// previewed rolls back the transaction a preview judges in.
var errPreviewed = errors.New("previewed")

// Preview judges s, from a suggester outside Themis, and governs it, as
// Submit and governing would, through the inbox's Governor: the same
// judging, the same decision, the same words. It keeps nothing: no
// suggestion, no change of state, nothing queued, nothing counted against
// the suggester. An *Error says what keeps the suggestion from being judged
// at all.
func (b *Inbox) Preview(ctx context.Context, from Suggester, s Suggestion) (Preview, error) {
	gov := b.Governor
	if gov == nil {
		return Preview{}, errors.New("no governor to preview a fix with")
	}
	if !from.outside() {
		return Preview{}, refuse(Forbidden, "only a suggester outside Themis previews a fix")
	}
	if err := wellFormed(s); err != nil {
		return Preview{}, err
	}
	in, err := b.inScope(ctx, from, s.Incident)
	if err != nil {
		return Preview{}, err
	}
	s.Provenance = attributed(from, s.Provenance)
	out := Preview{Incident: in, Matched: b.Config.Current().Matched(in.Profiles())}
	var r *rejection
	err = b.Store.InTx(ctx, func(tx pgx.Tx) error {
		p, invalid := b.proposal(in.ID, s)
		out.Proposal = p
		var err error
		r, err = b.judge(ctx, tx, from, subject{incidentID: in.ID, target: in.Target, profiles: out.Matched, names: in.Profiles(), state: in.State}, p, invalid)
		if err != nil {
			return err
		}
		return errPreviewed
	})
	if !errors.Is(err, errPreviewed) {
		return Preview{}, err
	}
	if r != nil {
		out.Proposal.Status, out.Proposal.RejectionReason, out.Proposal.Message = store.ProposalRejected, r.reason.String(), r.message
		out.Refused = r.message
		return out, nil
	}
	j, err := gov.Judge(ctx, in, out.Matched, out.Proposal)
	if err != nil {
		return Preview{}, err
	}
	out.Judgement = &j
	switch {
	case j.Refused != "":
		out.Refused = j.Refused
	case j.Decided.Decision.Outcome == policy.Deny:
		out.Refused = j.Decided.Decision.Reason
	}
	return out, nil
}
