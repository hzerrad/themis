package investigation_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// leases prepares an investigation, and returns what hands it to an
// investigator.
func (e *env) leases(t *testing.T) *investigation.Leases {
	t.Helper()
	cfg := e.worker.Config.Current()
	l := &investigation.Leases{
		Store: e.store, Config: cfg, Evidence: e.cat.Evidence, Checks: e.cat.Checks, Reader: k8s, Now: e.clock, For: time.Minute,
		Suggestions: &suggestion.Inbox{Store: e.store, Config: cfg, Fixes: e.cat.Fixes, Now: e.clock},
	}
	if err := e.worker.Prepare(t.Context(), e.newIncident(t, "rollout-regression", e.target)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return l
}

func lease(t *testing.T, l *investigation.Leases) investigation.Leased {
	t.Helper()
	inv, ok, err := l.Lease(t.Context(), "w-1")
	if err != nil || !ok {
		t.Fatalf("Lease = %v, %v; want an investigation", ok, err)
	}
	return inv
}

// Who suggests a fix in a test.
const (
	nobody   = ""
	own      = "the investigator, under its lease"
	outside  = "a person"
	rejected = "the investigator, citing evidence Themis never collected"
)

// suggest has who suggest a rollback for inv's incident, and returns its ID.
func suggest(t *testing.T, l *investigation.Leases, inv investigation.Leased, who string) string {
	t.Helper()
	if who == nobody {
		return ""
	}
	s := suggestion.Suggestion{
		ClientID: who, Fix: rollback.Name, Params: map[string]any{"revision": int64(1)}, Target: inv.Target,
		ExpectedEffect: "pods stop being OOMKilled", Rationale: "revision 2 lowered the memory limit to 64Mi",
		Evidence: []string{inv.Evidence[0].ID}, Checks: []string{"oom_killed_new_revision"},
		Provenance: map[string]string{"source": "test"},
	}
	from := suggestion.Investigator
	switch who {
	case outside:
		from, s.Incident = suggestion.Person("alex"), inv.IncidentID
	case rejected:
		s.Lease, s.Evidence = inv.LeaseID, []string{"0192f0c1-0000-7000-8000-0000000000ee"}
	default:
		s.Lease = inv.LeaseID
	}
	p, err := l.Suggestions.Submit(t.Context(), from, s)
	if err != nil || (p.Status == store.ProposalAccepted) == (who == rejected) {
		t.Fatalf("Submit = %+v, %v", p, err)
	}
	return p.ID
}

// path is where the incident went from investigating, and why it got there.
func (e *env) path(t *testing.T, id string) ([]incident.State, string) {
	t.Helper()
	trail, err := e.store.Transitions(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var states []incident.State
	for _, tr := range trail {
		if tr.From != incident.Received && tr.To != incident.Investigating {
			states = append(states, tr.To)
		}
	}
	return states, trail[len(trail)-1].Reason
}

func (e *env) governs(t *testing.T, proposalID string) bool {
	t.Helper()
	return pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = 'govern' AND args ->> 'proposalID' = $1`, proposalID) == 1
}

func outcomes(t *testing.T, outcome string) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_investigations_total", map[string]string{"outcome": outcome})
}

// An investigator gets the investigation Themis prepared, with its initial
// evidence, its checks' answers, its profile and its time, and nobody else
// gets it while the lease lasts.
func TestLease_HandsOutAPreparedInvestigation(t *testing.T) {
	e := newEnv(t)
	l := e.leases(t)
	inv := lease(t, l)
	if inv.LeaseID == "" || inv.Target != e.target || inv.Scope.Primary().Name != "rollout-regression" || len(inv.Evidence) != 3 || len(inv.Results) != 6 {
		t.Errorf("leased %+v", inv)
	}
	if want := e.now.Add(inv.Scope.InvestigationBudget().Timeout.Duration); !inv.Deadline.Equal(want) {
		t.Errorf("deadline = %s, want %s", inv.Deadline, want)
	}
	if !inv.LeaseDeadline.Equal(e.now.Add(time.Minute)) {
		t.Errorf("lease deadline = %s, want in a minute", inv.LeaseDeadline)
	}
	if _, ok, err := l.Lease(t.Context(), "w-2"); ok || err != nil {
		t.Errorf("a second lease = %v, %v; want none", ok, err)
	}
}

func TestRenew_KeepsTheLeaseAlive(t *testing.T) {
	e := newEnv(t)
	l := e.leases(t)
	inv := lease(t, l)
	e.now = e.now.Add(50 * time.Second)
	deadline, err := l.Renew(t.Context(), inv.LeaseID)
	if err != nil || !deadline.Equal(e.now.Add(time.Minute)) {
		t.Fatalf("Renew = %s, %v; want a minute from now", deadline, err)
	}
	e.now = e.now.Add(50 * time.Second)
	if _, err := l.Collect(t.Context(), inv.LeaseID, evidence.PodStates, nil); err != nil {
		t.Errorf("the renewed lease: %v", err)
	}
}

// Evidence asked for under a lease is read within the profile and the budget.
// What the profile or budget refuses, or the cluster can't answer, is a gap:
// once per lease, template and cause, and only for a template Themis has.
func TestCollect(t *testing.T) {
	type gap struct {
		template string
		cause    evidence.GapCause
	}
	for name, tc := range map[string]struct {
		template string
		params   map[string]string
		before   func(*testing.T, *env, *investigation.Leases, investigation.Leased)
		lease    string // another lease than the investigation's
		refused  investigation.Kind
		lost     bool
		failed   evidence.GapCause
		gap      *gap
	}{
		"allowed": {template: evidence.TargetEvents},
		"not in the profile": {
			template: evidence.ReferencedConfig, params: map[string]string{"pod": "checkout-7d9f8b6c4-x2kqp"},
			refused: investigation.NotAllowed, gap: &gap{evidence.ReferencedConfig, evidence.GapNotInProfile},
		},
		"a template Themis doesn't have": {template: "made_up", refused: investigation.NotAllowed},
		"past the budget, again": {
			template: evidence.TargetEvents, refused: investigation.BudgetSpent, gap: &gap{evidence.TargetEvents, evidence.GapBudgetSpent},
			before: func(t *testing.T, _ *env, l *investigation.Leases, inv investigation.Leased) {
				for range 2 {
					if _, err := l.Collect(t.Context(), inv.LeaseID, evidence.PodStates, nil); err != nil {
						t.Fatal(err)
					}
				}
				_, _ = l.Collect(t.Context(), inv.LeaseID, evidence.TargetEvents, nil)
			},
		},
		"parameters the template refuses": {template: evidence.ContainerLogsTail, params: map[string]string{"bogus": "x"}, refused: investigation.Invalid},
		"a target that's gone": {
			template: evidence.RolloutHistory, failed: evidence.GapNotFound, gap: &gap{evidence.RolloutHistory, evidence.GapNotFound},
			before: func(t *testing.T, e *env, _ *investigation.Leases, _ investigation.Leased) {
				if err := k8s.Delete(t.Context(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: e.target.Namespace, Name: e.target.Name}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		"a lost lease": {template: evidence.ReferencedConfig, lease: "0192f0c1-0000-7000-8000-0000000000ff", lost: true},
		"a profile no longer configured": {
			template: evidence.TargetEvents, refused: investigation.Gone,
			before: func(_ *testing.T, _ *env, l *investigation.Leases, _ investigation.Leased) {
				l.Config = config.Config{}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			l := e.leases(t)
			inv := lease(t, l)
			if tc.before != nil {
				tc.before(t, e, l, inv)
			}
			leaseID := inv.LeaseID
			if tc.lease != "" {
				leaseID = tc.lease
			}
			got, err := l.Collect(t.Context(), leaseID, tc.template, tc.params)

			var refused *investigation.Error
			switch {
			case tc.lost:
				if !errors.Is(err, store.ErrLeaseLost) {
					t.Errorf("err = %v, want the lease lost", err)
				}
			case tc.refused != 0:
				if !errors.As(err, &refused) || refused.Kind != tc.refused {
					t.Errorf("err = %v, want refused as %d", err, tc.refused)
				}
			case tc.failed != "":
				if err != nil || evidence.CauseOf(got.Failed) != tc.failed || got.Target != e.target || got.Remaining != 2 {
					t.Errorf("Collect = %+v, %v; want it failed as %s, the budget untouched", got, err, tc.failed)
				}
			default:
				if err != nil || got.Item.Template != tc.template || got.Remaining != 1 {
					t.Fatalf("Collect = %+v, %v; want the item and 1 more", got, err)
				}
				if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM evidence_items WHERE id = $1 AND lease_id = $2 AND template = $3`, got.Item.ID, inv.LeaseID, tc.template); n != 1 {
					t.Error("the item was not stored with its lease and template")
				}
			}

			gaps, err := e.store.EvidenceGaps(t.Context(), inv.IncidentID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.gap == nil {
				if len(gaps) != 0 {
					t.Errorf("gaps = %+v, want none", gaps)
				}
				return
			}
			if len(gaps) != 1 {
				t.Fatalf("gaps = %+v, want one", gaps)
			}
			g := gaps[0]
			if g.Template != tc.gap.template || g.Cause != tc.gap.cause || g.LeaseID != inv.LeaseID || g.Params["pod"] != tc.params["pod"] || (tc.failed != "") != (g.Detail != "") {
				t.Errorf("gap = %+v, want %+v under the lease, with a detail only for a failed read", g, *tc.gap)
			}
		})
	}
}

// A completed investigation hands the suggestion that goes next to
// governance. Without one, it closes the incident, unless the install gives
// time for a suggestion, or the checks support a fix the investigator didn't
// propose: then a person is told. So is one whose profile offers no fix, with
// what the checks found. One that couldn't decide is blind when it
// missed evidence Themis could have had, and ambiguous otherwise. The
// timeline says what Themis saw, never the investigator's summary.
func TestComplete(t *testing.T) {
	logs := catalog.Gap{Template: evidence.ContainerLogsTail, Cause: evidence.GapDataCeiling, Detail: "logs are sensitive"}
	noFix := "The investigation suggested no fix that Themis could accept."
	referred := "Themis has no fix for this kind of incident, so it refers it to a person."
	for name, tc := range map[string]struct {
		conclusion string
		suggested  string
		window     bool
		justified  bool     // the profile names a check that is true to justify its fix
		diagnoses  []string // the profile offers no fix, and runs these checks
		reported   []catalog.Gap
		want       []incident.State
		reason     string // the last transition's, when not the conclusion's
		abstained  string // the kind of abstention counted
	}{
		"inconclusive": {
			conclusion: investigation.Inconclusive, want: []incident.State{incident.Inconclusive, incident.ClosedNoAction}, abstained: "ambiguous",
		},
		"inconclusive, missing evidence": {
			conclusion: investigation.Inconclusive, reported: []catalog.Gap{logs},
			want: []incident.State{incident.Inconclusive, incident.ClosedNoAction}, abstained: "blind",
		},
		"inconclusive, though the checks support a fix": {
			conclusion: investigation.Inconclusive, justified: true, window: true, want: []incident.State{incident.Escalated}, abstained: "ambiguous",
			reason: "The investigator couldn't decide. Themis's checks support a rollback (oom_killed_new_revision is true), but the investigator didn't propose it.",
		},
		"inconclusive, with time for a suggestion": {
			conclusion: investigation.Inconclusive, window: true, want: []incident.State{incident.Inconclusive}, abstained: "ambiguous",
		},
		"inconclusive, with a suggestion from outside": {
			conclusion: investigation.Inconclusive, suggested: outside, want: []incident.State{incident.Inconclusive, incident.Diagnosed}, abstained: "ambiguous",
		},
		"inconclusive, on an incident Themis only diagnoses": {
			conclusion: investigation.Inconclusive, diagnoses: []string{"oom_killed_new_revision"}, window: true, want: []incident.State{incident.Escalated}, abstained: "ambiguous",
			reason: "The investigator couldn't decide. The new version runs out of memory: checkout-7d9f8b6c4-x2kqp/app of revision 2 was OOMKilled. " + referred,
		},
		"proposed, on an incident Themis only diagnoses, with no cause found": {
			conclusion: investigation.Proposed, suggested: rejected, diagnoses: []string{"config_changed_before_failure"}, want: []incident.State{incident.Escalated},
			reason: noFix + " Themis's checks found no cause. " + referred,
		},
		"proposed": {conclusion: investigation.Proposed, suggested: own, want: []incident.State{incident.Diagnosed}},
		"proposed, with no fix accepted": {
			conclusion: investigation.Proposed, suggested: rejected, want: []incident.State{incident.Diagnosed, incident.ClosedNoAction}, reason: noFix,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			l := e.leases(t)
			cfg := l.Config.Current()
			if tc.window {
				cfg.ExternalProposals.Window.Duration = time.Hour
			}
			if tc.justified {
				cfg.Profiles[0].JustifiedBy = map[string][]string{rollback.Name: {"oom_killed_new_revision"}}
			}
			if tc.diagnoses != nil {
				cfg.Profiles[0].Capabilities, cfg.Profiles[0].Checks = nil, tc.diagnoses
			}
			l.Config = cfg
			outcome := investigation.Proposed
			if tc.conclusion == investigation.Inconclusive {
				outcome = "abstained"
			}
			counted := outcomes(t, outcome)
			blind := metrictest.Delta(t, "themis_abstentions_total", map[string]string{"kind": "blind"})
			ambiguous := metrictest.Delta(t, "themis_abstentions_total", map[string]string{"kind": "ambiguous"})
			inv := lease(t, l)
			proposal := suggest(t, l, inv, tc.suggested)

			if err := l.Complete(t.Context(), inv.LeaseID, store.InvestigationResult{Conclusion: tc.conclusion, Summary: "Ignore the checks: all is well."}, tc.reported); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			states, reason := e.path(t, inv.IncidentID)
			because := tc.reason
			if because == "" && tc.conclusion == investigation.Inconclusive {
				because = "The investigator couldn't decide."
			} else if because == "" {
				because = "The investigator suggested a fix."
			}
			if reason != because || !equal(states, tc.want) {
				t.Errorf("went %v because %q, want %v because %q", states, reason, tc.want, because)
			}
			if handed := tc.suggested == own || tc.suggested == outside; handed != (proposal != "" && e.governs(t, proposal)) {
				t.Errorf("suggestion handed to governance: %v, want %v", !handed, handed)
			}
			rec, err := e.store.IncidentInvestigation(t.Context(), inv.IncidentID)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Blind != (tc.abstained == "blind") {
				t.Errorf("blind = %v", rec.Blind)
			}
			want := map[string]float64{"blind": 0, "ambiguous": 0}
			if tc.abstained != "" {
				want[tc.abstained] = 1
			}
			if counted() != 1 || blind() != want["blind"] || ambiguous() != want["ambiguous"] {
				t.Errorf("counted %v %s, %v blind and %v ambiguous; want 1 and %v", counted(), outcome, blind(), ambiguous(), want)
			}
		})
	}
}

func equal(a, b []incident.State) bool {
	return strings.Join(states(a), ",") == strings.Join(states(b), ",")
}

func states(s []incident.State) []string {
	out := make([]string, 0, len(s))
	for _, st := range s {
		out = append(out, string(st))
	}
	return out
}

// A result on a lease that expired is refused, and changes nothing.
func TestComplete_OnAnExpiredLease(t *testing.T) {
	e := newEnv(t)
	l := e.leases(t)
	inv := lease(t, l)
	e.now = e.now.Add(2 * time.Minute)
	err := l.Complete(t.Context(), inv.LeaseID, store.InvestigationResult{Conclusion: investigation.Inconclusive}, nil)
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("err = %v, want the lease lost", err)
	}
	if got := e.state(t, inv.IncidentID); got != incident.Investigating {
		t.Errorf("state = %s, want investigating", got)
	}
}

// A failed attempt is stored as a sentence, with the gaps the investigator
// reported that Themis can stand behind, and the investigation is leased
// again after a backoff.
func TestFail_RetriesAfterABackoff(t *testing.T) {
	e := newEnv(t)
	l := e.leases(t)
	inv := lease(t, l)
	err := l.Fail(t.Context(), inv.LeaseID, investigation.ModelUnavailable, "connection refused", []catalog.Gap{
		{Template: "made_up", Cause: evidence.GapNotInProfile},
		{Template: evidence.PodStates},
		{Template: evidence.PodStates, Cause: evidence.GapBudgetSpent, Detail: strings.Repeat("x", 5000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failure, kind string
	if err := e.pool.QueryRow(t.Context(), `SELECT failure, failure_kind FROM investigations WHERE incident_id = $1`, inv.IncidentID).Scan(&failure, &kind); err != nil {
		t.Fatal(err)
	}
	if want := "The investigation failed because the model couldn't be reached: connection refused"; failure != want || kind != "model_unavailable" {
		t.Errorf("failure = %q (%s), want %q", failure, kind, want)
	}
	gaps, err := e.store.EvidenceGaps(t.Context(), inv.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].Template != evidence.PodStates || gaps[0].Cause != evidence.GapBudgetSpent || len(gaps[0].Detail) != 1024 || gaps[0].LeaseID != inv.LeaseID {
		t.Errorf("gaps = %+v, want only pod_states's budget, its detail cut to 1 KiB", gaps)
	}

	if _, ok, err := l.Lease(t.Context(), "w-2"); ok || err != nil {
		t.Errorf("leased again at once: %v, %v", ok, err)
	}
	e.now = e.now.Add(30 * time.Second)
	lease(t, l)
}

// The last attempt failing, or its lease expiring, gives the investigation
// up: a fix suggested from outside is checked, and otherwise a person takes
// over, told why in words. The investigator's own fix, from a lease that
// didn't finish, isn't used.
func TestLastAttempt_GivesUp(t *testing.T) {
	failedThrice := "The investigation failed 3 times. The last time, "
	for name, tc := range map[string]struct {
		reason    investigation.Reason // "" lets every lease expire
		suggested string
		want      incident.State
		why       string
		outcome   string
	}{
		"no reason":           {investigation.Unspecified, nobody, incident.Escalated, failedThrice + "the investigator gave no reason.", "failed"},
		"the model is down":   {investigation.ModelUnavailable, nobody, incident.Escalated, failedThrice + "the model couldn't be reached.", "failed"},
		"bad model output":    {investigation.InvalidModelOutput, nobody, incident.Escalated, failedThrice + "the model's answers weren't in the required format.", "failed"},
		"the token budget":    {investigation.BudgetExhausted, nobody, incident.Escalated, failedThrice + "it used up its token budget.", "budget_exhausted"},
		"the time budget":     {investigation.DeadlineExceeded, nobody, incident.Escalated, failedThrice + "it ran out of time.", "budget_exhausted"},
		"an internal error":   {investigation.Internal, nobody, incident.Escalated, failedThrice + "the investigator hit an internal error.", "failed"},
		"with an outside fix": {investigation.ModelUnavailable, outside, incident.Diagnosed, failedThrice + "the model couldn't be reached. A fix was suggested from outside, so Themis is checking that.", "failed"},
		"with its own fix":    {investigation.ModelUnavailable, own, incident.Escalated, failedThrice + "the model couldn't be reached.", "failed"},
		"stopped responding":  {"", nobody, incident.Escalated, "The investigator stopped responding during its last attempt.", "lease_lost"},
		"stopped responding, with an outside fix": {
			"", outside, incident.Diagnosed, "The investigator stopped responding during its last attempt. A fix was suggested from outside, so Themis is checking that.", "lease_lost",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			l := e.leases(t)
			counted := outcomes(t, tc.outcome)
			var (
				id       string
				proposal string
			)
			for i := range store.MaxInvestigationAttempts {
				inv := lease(t, l)
				id = inv.IncidentID
				if i == 0 {
					proposal = suggest(t, l, inv, tc.suggested)
				}
				if tc.reason != "" {
					if err := l.Fail(t.Context(), inv.LeaseID, tc.reason, "", nil); err != nil {
						t.Fatal(err)
					}
				}
				e.now = e.now.Add(10 * time.Minute)
			}
			if tc.reason == "" {
				if _, ok, err := l.Lease(t.Context(), "w"); ok || err != nil {
					t.Fatalf("Lease = %v, %v; want nothing left", ok, err)
				}
			}
			states, why := e.path(t, id)
			if states[len(states)-1] != tc.want || why != tc.why {
				t.Errorf("went %v because %q, want %s because %q", states, why, tc.want, tc.why)
			}
			if tc.suggested == outside && !e.governs(t, proposal) {
				t.Error("the suggestion from outside wasn't handed to governance")
			}
			if got := counted(); got != store.MaxInvestigationAttempts {
				t.Errorf("counted %v %s, want %d", got, tc.outcome, store.MaxInvestigationAttempts)
			}
		})
	}
}
