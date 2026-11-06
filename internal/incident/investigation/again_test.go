package investigation_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/store"
)

// denied has a person suggest a rollback to revision 1, the investigator
// find nothing better using tokens of a budget of 5,000, and alice deny the
// rollback as awaiting her approval. The suggestion, governed, never goes
// again.
func (e *env) denied(t *testing.T, l *investigation.Leases) store.Investigation {
	t.Helper()
	ctx := t.Context()
	inv := lease(t, l)
	proposal := suggest(t, l, inv, outside)
	if err := l.Complete(ctx, inv.LeaseID, store.InvestigationResult{
		Conclusion: investigation.Inconclusive, Usage: []byte(`{"inputTokens": "1000", "outputTokens": "200", "tokenBudget": "5000"}`),
	}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// As governance leaves it: the rollback waits for a person.
	var action string
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		if err := e.store.TransitionPath(ctx, tx, inv.IncidentID, "test", "test", incident.Diagnosed, incident.Proposed, incident.AwaitingApproval); err != nil {
			return err
		}
		var err error
		action, err = e.store.InsertAction(ctx, tx, store.Action{
			IncidentID: inv.IncidentID, ProposalID: proposal, Capability: rollback.Name, CapabilityVersion: "1",
			Target: digest.TargetIdentity{Group: "apps", Kind: "Deployment", Namespace: e.target.Namespace, Name: e.target.Name, UID: e.target.UID},
			Params: map[string]any{"revision": int64(1)}, State: store.ActionAwaitingApproval, Digest: "sha256:test",
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := governance.Deny(ctx, e.store, action, "alice", "The new revision carries a schema migration.", e.now); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	return inv.Investigation
}

// After a denial, a person has the incident investigated again. The
// investigator gets the fix denied with alice's reason, as she wrote it, and
// what the incident's investigations used of their token budget. When it
// finds nothing, the incident waits for a suggested fix again, though the
// checks still support the rollback alice denied.
func TestAgain(t *testing.T) {
	e := newEnv(t)
	l := e.leases(t)
	first := e.denied(t, l)
	again := &investigation.Again{Store: e.store, Config: l.Config}
	in, err := e.store.Incident(t.Context(), first.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if o, err := again.Offer(t.Context(), in); err != nil || o != (investigation.Offer{Offered: true}) {
		t.Fatalf("Offer = %+v, %v; want offered", o, err)
	}
	if err := again.Start(t.Context(), in.ID, "alice"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := again.Start(t.Context(), in.ID, "alice"); !errors.As(err, new(*investigation.Refusal)) {
		t.Errorf("starting it twice: %v, want refused", err)
	}
	if _, reason := e.path(t, in.ID); reason != "Investigation started again by alice." {
		t.Errorf("timeline says %q", reason)
	}

	inv := lease(t, l)
	if inv.IncidentID != in.ID || inv.LeaseID == first.LeaseID || inv.TokensUsed != 1200 || len(inv.Evidence) != 3 {
		t.Errorf("leased %s under %s with %d tokens used and %d pieces of evidence", inv.IncidentID, inv.LeaseID, inv.TokensUsed, len(inv.Evidence))
	}
	if d := inv.Denials; len(d) != 1 || d[0].Denier != "alice" || d[0].Reason != "The new revision carries a schema migration." ||
		d[0].Capability != rollback.Name || d[0].Params["revision"] != int64(1) {
		t.Errorf("denials = %+v, want alice's of the rollback, with her reason", d)
	}
	if err := l.Complete(t.Context(), inv.LeaseID, store.InvestigationResult{
		Conclusion: investigation.Inconclusive, Usage: []byte(`{"inputTokens": "3500", "outputTokens": "500", "tokenBudget": "5000"}`),
	}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if states, reason := e.path(t, in.ID); states[len(states)-1] != incident.AwaitingSuggestion || reason != "No fix proposed." {
		t.Errorf("went %v, saying %q; want back to awaiting_suggestion", states, reason)
	}
	if got, err := e.store.IncidentInvestigation(t.Context(), in.ID); err != nil || got.Conclusion != investigation.Inconclusive {
		t.Errorf("latest investigation = %+v, %v; want the second", got, err)
	}

	// The two used 5,200 of the incident's 5,000 tokens.
	in, _ = e.store.Incident(t.Context(), in.ID)
	spent := "This incident's investigations used 5,200 of its 5,000 tokens."
	if o, err := again.Offer(t.Context(), in); err != nil || o != (investigation.Offer{Offered: true, Refused: spent}) {
		t.Errorf("Offer = %+v, %v; want refused for the budget", o, err)
	}
	var refusal *investigation.Refusal
	if err := again.Start(t.Context(), in.ID, "alice"); !errors.As(err, &refusal) || refusal.Reason != spent {
		t.Errorf("Start = %v, want refused for the budget", err)
	}

	// Without an investigator, nobody is offered it.
	cfg := l.Config.Current()
	cfg.Investigators = config.Investigators{Disabled: true}
	none := &investigation.Again{Store: e.store, Config: cfg}
	if o, err := none.Offer(t.Context(), in); err != nil || o.Offered {
		t.Errorf("Offer without an investigator = %+v, %v", o, err)
	}
	if err := none.Start(t.Context(), in.ID, "alice"); !errors.As(err, &refusal) || refusal.Reason != "No investigator is installed." {
		t.Errorf("Start without an investigator = %v", err)
	}
}
