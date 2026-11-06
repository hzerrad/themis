package investigation

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// Again has an incident investigated again when a person asks, while it
// waits for a suggested fix: after they denied its fix, or a fix suggested
// was refused. The investigator gets what the incident has, and the fixes
// people denied with their reasons. The new investigation draws on the
// incident's token budget: what the earlier ones used counts. Its zero
// value is not ready: set Store and Config.
type Again struct {
	Store  *store.Store
	Config config.Provider
}

// Offer is whether a person can have an incident investigated again.
type Offer struct {
	// Offered is set while the incident waits for a suggestion and an
	// investigator is installed.
	Offered bool
	// Refused says why it can't be, when offered, such as a token budget
	// spent: "This incident's investigations used 200,312 of its 200,000
	// tokens."
	Refused string
}

// Refusal is why an incident can't be investigated again, in a sentence
// to follow "Refused.".
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

// Offer says whether a person can have in investigated again.
func (a *Again) Offer(ctx context.Context, in store.Incident) (Offer, error) {
	return a.offer(ctx, a.Store.DB(), in.ID, in.State, in.Profiles())
}

func (a *Again) offer(ctx context.Context, q store.Querier, incidentID string, state incident.State, profiles []string) (Offer, error) {
	cfg := a.Config.Current()
	if state != incident.AwaitingSuggestion || cfg.Investigators.Disabled {
		return Offer{}, nil
	}
	if len(cfg.Matched(profiles)) == 0 {
		return Offer{Offered: true, Refused: fmt.Sprintf("Themis no longer has %s this incident matched.", config.Named(profiles))}, nil
	}
	tokens, err := a.Store.IncidentTokens(ctx, q, incidentID)
	if err != nil {
		return Offer{}, err
	}
	if tokens.Spent() {
		return Offer{Offered: true, Refused: fmt.Sprintf("This incident's investigations used %s of its %s tokens.", thousands(tokens.Used), thousands(tokens.Budget))}, nil
	}
	return Offer{Offered: true}, nil
}

// Start has the incident investigated again, as by asked. A *Refusal says
// why it can't be.
func (a *Again) Start(ctx context.Context, incidentID, by string) error {
	return a.Store.InTx(ctx, func(tx pgx.Tx) error {
		state, err := a.Store.LockIncident(ctx, tx, incidentID)
		if err != nil {
			return err
		}
		in, err := a.Store.Incident(ctx, incidentID)
		if err != nil {
			return err
		}
		o, err := a.offer(ctx, tx, incidentID, state, in.Profiles())
		switch {
		case err != nil:
			return err
		case !o.Offered && a.Config.Current().Investigators.Disabled:
			return &Refusal{Reason: "No investigator is installed."}
		case !o.Offered:
			return &Refusal{Reason: "The incident isn't waiting for a fix."}
		case o.Refused != "":
			return &Refusal{Reason: o.Refused}
		}
		if _, err := a.Store.QueueInvestigation(ctx, tx, incidentID); err != nil {
			return err
		}
		return a.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: incidentID, From: incident.AwaitingSuggestion, To: incident.Investigating,
			Reason: "Investigation started again by " + by + ".", Actor: by,
		})
	})
}

// thousands writes n with a comma between each group of three digits:
// 200,000.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := 0
	if n < 0 {
		start = 1
	}
	for i := len(s) - 3; i > start; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
