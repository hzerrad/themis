package governance_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/store"
)

// Which changes to roll back is where text planted in the evidence can
// steer the investigator, so the shipped policy refuses its partial rollback
// while any of the incident's evidence holds text aimed at Themis, an item
// asked for after the checks ran included. A person may still suggest one,
// and with no such text the investigator's waits for approval.
func TestJudge_PartialRollbackOnFlaggedEvidence(t *testing.T) {
	const limit = "the memory limit of container app changed from 256Mi to 64Mi"
	for _, tt := range []struct {
		name    string
		flagged bool
		person  bool
		want    policy.Outcome
		rule    string
	}{
		{name: "text aimed at Themis", flagged: true, want: policy.Deny, rule: "partial-rollback-not-from-flagged-evidence"},
		{name: "a person's suggestion", flagged: true, person: true, want: policy.RequireApproval, rule: "rollback-needs-approval"},
		{name: "no text aimed at Themis", want: policy.RequireApproval, rule: "rollback-needs-approval"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			id, _ := e.diagnosed(t, "rollout-regression", 1, facts(check.True))
			if tt.flagged {
				e.collectedLater(t, id, evidence.Item{
					ID: "0192f0c1-0000-7000-8000-00000000000b", Template: evidence.ContainerLogsTail, Data: []byte(`{}`), CollectedAt: now,
					Classification: evidence.ClassificationSensitive, Suspicious: []evidence.Suspicion{{Reason: evidence.ReasonInstructions, At: "/logs"}},
				})
			}
			in, err := e.store.Incident(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			p := store.Proposal{
				IncidentID: id, LeaseID: "0192f0c1-0000-7000-8000-000000000001", Capability: rollback.PartialName,
				Params: map[string]any{"revision": int64(1), "changes": []any{limit}}, Provenance: map[string]string{"source": "themis-intelligence"}, Authenticated: true,
			}
			if tt.person {
				p.LeaseID, p.Provenance = "", map[string]string{"source": "person:alice"}
			}
			j, err := e.worker.Judge(t.Context(), in, config.Matched{shippedProfile(t)}, p)
			if err != nil {
				t.Fatal(err)
			}
			if d := j.Decided.Decision; d.Outcome != tt.want || d.PolicyID != tt.rule {
				t.Errorf("decision = %s by %q (%s), want %s by %q", d.Outcome, d.PolicyID, d.Reason, tt.want, tt.rule)
			}
			const refusal = "Refused. The evidence holds text aimed at Themis, so a partial rollback from the investigator isn't taken. The full rollback is still on offer."
			if d := j.Decided.Decision; tt.want == policy.Deny && !strings.HasPrefix(d.Reason, refusal) {
				t.Errorf("reason = %q, want it to start %q", d.Reason, refusal)
			}
		})
	}
}

// A partial rollback is justified and decided by the same checks and rules
// as a rollback, and refused when it names anything but a change the latest
// rollout made. The approver sees the digests of an image it rolls back.
// When the checks support both and nobody suggested either, a person is told
// they are one choice.
func TestDecide_PartialRollback(t *testing.T) {
	shipped, words := shippedProfile(t), shippedChecks(t)
	const limit = "the memory limit of container app changed from 256Mi to 64Mi"
	const image = "the image of container app changed from example/checkout:2.3.1 to example/checkout:2.4.0"
	for _, tt := range []struct {
		name    string
		checks  []check.Result
		changes []any
		want    policy.Outcome
		rule    string
		refusal string
	}{
		{name: "a bad rollout", checks: facts(check.True), changes: []any{limit}, want: policy.RequireApproval, rule: "rollback-needs-approval"},
		{name: "the rollout didn't bring the failures", checks: set(facts(check.True), 4, check.False), changes: []any{limit}, want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		{name: "the failures started before the rollout", checks: facts(check.False), changes: []any{limit}, want: policy.Deny, rule: governance.UnjustifiedRule},
		{
			name: "a change the rollout didn't make", checks: facts(check.True), changes: []any{limit, "the environment variable DB_PASSWORD of container app changed"},
			refusal: `"the environment variable DB_PASSWORD of container app changed" isn't one of the changes revision 2 made to what the pods run.`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var decider governance.Decider
			d, err := decider.Decide(t.Context(), k8s, governance.Fix{
				Incident: "incident-1", Target: cluster(t), Profiles: config.Matched{shipped}, Checks: tt.checks, Words: words,
				Capability: rollback.Partial{}, Params: map[string]any{"revision": int64(1), "changes": tt.changes}, By: investigator,
			})
			if tt.refusal != "" {
				if refusal, ok := errors.AsType[*capability.Refusal](err); !ok || !strings.HasPrefix(refusal.Reason, tt.refusal) {
					t.Fatalf("err = %v, want the refusal %q", err, tt.refusal)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Decision.Outcome != tt.want || d.Decision.PolicyID != tt.rule {
				t.Errorf("decision = %s by %q (%s), want %s by %q", d.Decision.Outcome, d.Decision.PolicyID, d.Decision.Reason, tt.want, tt.rule)
			}
		})
	}

	var decider governance.Decider
	d, err := decider.Decide(t.Context(), k8s, governance.Fix{
		Incident: "incident-1", Target: cluster(t), Profiles: config.Matched{shipped}, Checks: facts(check.True), Words: words,
		Capability: rollback.Partial{}, Params: map[string]any{"revision": int64(1), "changes": []any{image}}, By: investigator,
	})
	if err != nil {
		t.Fatal(err)
	}
	images, _ := d.Action.BlastRadius["images"].(map[string]any)
	if app, _ := images["app"].(map[string]any); app["after"] != "sha256:"+strings.Repeat("1", 64) || d.Action.BlastRadius["replicas"] != int64(3) {
		t.Errorf("blast radius %v, want revision 1's digest beside the replicas", d.Action.BlastRadius)
	}

	note, err := decider.Unproposed(config.Matched{shipped}, catalog.Fixes(), facts(check.True), words, "shop")
	if err != nil || note != "The checks support a rollback or a partial rollback (the failures started after the rollout)." {
		t.Errorf("Unproposed = %q, %v", note, err)
	}
}
