package governance_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/catalog/capability/probes"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// waiting creates an incident that waits for a suggested fix, with its
// evidence and checks, as one with no investigator does, and returns its ID
// and its evidence's.
func (e *env) waiting(t *testing.T, results []check.Result) (string, string) {
	t.Helper()
	ctx := t.Context()
	var id string
	item := evidence.Item{ID: uuid.NewString(), Template: evidence.PodStates, Target: e.target, Data: []byte(`{"pods":[]}`), CollectedAt: now, Classification: evidence.ClassificationInternal}
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		if id, _, err = e.store.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{
			Fingerprint: "preview/" + uuid.NewString(), Profile: "rollout-regression", Target: e.target, Payload: []byte(`{}`),
		}); err != nil {
			return err
		}
		if err := e.store.TransitionPath(ctx, tx, id, "test", "test", incident.Received, incident.AwaitingSuggestion); err != nil {
			return err
		}
		return e.store.RecordInitialEvidence(ctx, tx, id, []evidence.Item{item}, results)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, item.ID
}

// kept counts what an incident keeps: suggestions, changes of state, actions
// and jobs queued.
func (e *env) kept(t *testing.T, id string) []int {
	t.Helper()
	return []int{
		pgtest.Count(t, e.pool, `SELECT count(*) FROM proposals WHERE incident_id = $1`, id),
		pgtest.Count(t, e.pool, `SELECT count(*) FROM transitions WHERE incident_id = $1`, id),
		pgtest.Count(t, e.pool, `SELECT count(*) FROM actions WHERE incident_id = $1`, id),
		pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE args->>'incidentID' = $1 OR args->>'IncidentID' = $1`, id),
		pgtest.Count(t, e.pool, `SELECT count(*) FROM predicate_results WHERE incident_id = $1 AND value = 'unknown'`, id),
	}
}

// A preview of a suggested fix is the same judgement the suggestion gets,
// through the same code, and keeps nothing: whatever the preview says, a
// person who then suggests the fix gets exactly that, accepted and put to a
// person by the same rule, or refused for the same reason, in the same words.
func TestPreview_JudgesAsASuggestionIs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		results  []check.Result
		fix      string
		params   map[string]any
		accepted bool
	}{
		{name: "a rollback the checks justify and policy puts to a person", results: predicates, fix: rollback.Name, params: map[string]any{"revision": int64(1)}, accepted: true},
		{name: "a rollback the checks don't justify", results: notJustified, fix: rollback.Name, params: map[string]any{"revision": int64(1)}},
		{name: "a rollback to a revision Kubernetes no longer keeps", results: predicates, fix: rollback.Name, params: map[string]any{"revision": int64(9)}},
		{name: "a fix no profile of the incident offers", results: predicates, fix: probes.Name, params: map[string]any{"container": "app", "liveness": map[string]any{"timeoutSeconds": int64(3)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			inbox := &suggestion.Inbox{Store: e.store, Config: e.worker.Config, Fixes: e.worker.Fixes, Checks: e.worker.Checks, Governor: e.worker, Now: e.worker.Now}
			id, ev := e.waiting(t, tc.results)
			s := suggestion.Suggestion{
				Incident: id, ClientID: "ui-1", Fix: tc.fix, Params: tc.params, Target: e.target,
				Rationale: "why", Evidence: []string{ev}, Provenance: map[string]string{"source": "web"},
			}
			before := e.kept(t, id)
			pv, err := inbox.Preview(t.Context(), suggestion.Person("alex"), s)
			if err != nil {
				t.Fatal(err)
			}
			if after := e.kept(t, id); !slices.Equal(before, after) {
				t.Fatalf("the preview kept something: %v, then %v", before, after)
			}
			if got := pv.Refused == ""; got != tc.accepted {
				t.Fatalf("accepted = %v (%q), want %v", got, pv.Refused, tc.accepted)
			}

			// The same suggestion, made.
			p, err := inbox.Submit(t.Context(), suggestion.Person("alex"), s)
			if err != nil {
				t.Fatal(err)
			}
			if p.Status == store.ProposalRejected {
				if pv.Refused != p.Message {
					t.Errorf("refused for %q, previewed %q", p.Message, pv.Refused)
				}
				return
			}
			e.govern(t, p.ID)
			trail, err := e.store.Transitions(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			last := trail[len(trail)-1]
			if tc.accepted {
				a := e.action(t, id)
				if last.To != incident.AwaitingApproval || a.DecisionReason != pv.Judgement.Decided.Decision.Reason ||
					pv.Judgement.Decided.Decision.Outcome != policy.RequireApproval {
					t.Errorf("governed to %s by %q; previewed %s by %q", last.To, a.DecisionReason, pv.Judgement.Decided.Decision.Outcome, pv.Judgement.Decided.Decision.Reason)
				}
				return
			}
			if last.To != incident.AwaitingSuggestion || last.Reason != pv.Refused {
				t.Errorf("governed to %s for %q; previewed %q", last.To, last.Reason, pv.Refused)
			}
		})
	}
}

// A preview names the checks that bore on a refusal: those whose answer
// alone made the rule match. An accepted fix names those the refusing rules
// read, which it got past.
func TestPreview_NamesTheChecksThatBore(t *testing.T) {
	e := shipped(t)
	inbox := &suggestion.Inbox{Store: e.store, Config: e.worker.Config, Fixes: e.worker.Fixes, Checks: e.worker.Checks, Governor: e.worker, Now: e.worker.Now}
	r := func(name string, v check.Truth) check.Result {
		return check.Result{Name: name, Version: "1", Value: v, Reason: name}
	}
	bad := []check.Result{
		r("oom_killed_new_revision", check.True), r("failures_came_with_rollout", check.True),
		r("config_changed_before_failure", check.True), r(check.GitOpsManaged, check.False),
	}
	id, ev := e.waiting(t, bad)
	s := suggestion.Suggestion{Incident: id, ClientID: "ui-1", Fix: rollback.Name, Params: map[string]any{"revision": int64(1)}, Target: e.target, Evidence: []string{ev}}
	pv, err := inbox.Preview(t.Context(), suggestion.Person("alex"), s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pv.Refused, "rollback-needs-a-bad-rollout") || !slices.Equal(pv.Judgement.Against, []string{"config_changed_before_failure"}) {
		t.Errorf("refused %q, against %v", pv.Refused, pv.Judgement.Against)
	}

	good := slices.Clone(bad)
	good[2] = r("config_changed_before_failure", check.False)
	id, ev = e.waiting(t, good)
	s.Incident, s.Evidence = id, []string{ev}
	if pv, err = inbox.Preview(t.Context(), suggestion.Person("alex"), s); err != nil {
		t.Fatal(err)
	}
	if pv.Refused != "" || !slices.Equal(pv.Judgement.Past, []string{"failures_came_with_rollout", "config_changed_before_failure"}) {
		t.Errorf("refused %q, past %v", pv.Refused, pv.Judgement.Past)
	}
}
