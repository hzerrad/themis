package suggestion_test

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// restart is a fix Themis doesn't ship with, at a version of its own.
type restart struct{ capability.Capability }

func (restart) Name() string                                      { return "test.restart" }
func (restart) Version() string                                   { return "7" }
func (restart) Validate(p map[string]any) (map[string]any, error) { return p, nil }

const profileName = "rollout-regression"

var now = time.Date(2026, 10, 27, 9, 20, 0, 0, time.UTC)

// env is an incident under investigation, its lease, and one item of
// evidence collected for it.
type env struct {
	inbox    *suggestion.Inbox
	store    *store.Store
	pool     *pgxpool.Pool
	incident string
	lease    string
	evidence string
	target   incident.TargetRef
}

// newEnv sets up an incident that matched two profiles: its primary one
// offers no fix, and the other offers fixes; nil offers a rollback and
// test.restart. A fix is judged by every profile the incident matched.
func newEnv(t *testing.T, fixes []string) *env {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := maps.Clone(cat.Fixes)
	registry["test.restart"] = restart{}
	if fixes == nil {
		fixes = []string{rollback.Name, "test.restart"}
	}
	e := &env{
		inbox: &suggestion.Inbox{
			Store: s, Fixes: registry, Now: func() time.Time { return now },
			Config: config.Config{Profiles: []config.Profile{{Name: "diagnosis"}, {Name: profileName, Capabilities: fixes}}},
		},
		store: s, pool: pool, evidence: uuid.NewString(),
		target: incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "u-checkout"},
	}
	err = s.InTx(t.Context(), func(tx pgx.Tx) error {
		if e.incident, _, err = s.CreateOrJoinIncident(t.Context(), tx, store.IncidentArgs{
			Fingerprint: "f", Profile: "diagnosis", AlsoMatched: []string{profileName}, Target: e.target, Payload: []byte(`{}`),
		}); err != nil {
			return err
		}
		if err := s.TransitionPath(t.Context(), tx, e.incident, "test", "test", incident.Received, incident.Investigating); err != nil {
			return err
		}
		item := evidence.Item{ID: e.evidence, Template: evidence.PodStates, Target: e.target, Data: []byte(`{}`), CollectedAt: now, Classification: evidence.ClassificationInternal}
		ran := []check.Result{{Name: "oom_killed_new_revision", Version: "1", Value: check.True, Reason: "x", EvidenceIDs: []string{e.evidence}}}
		if _, err := s.CreateInvestigation(t.Context(), tx, e.incident, []evidence.Item{item}, ran); err != nil {
			return err
		}
		inv, _, err := s.LeaseInvestigation(t.Context(), tx, "w", now, time.Minute)
		e.lease = inv.LeaseID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// request is a valid suggestion of a rollback under the lease, as the APIs
// receive it.
func (e *env) request(clientID string) *themisv1.SubmitProposalRequest {
	t := e.target
	return &themisv1.SubmitProposalRequest{
		LeaseId: e.lease, ClientProposalId: clientID,
		Target:           &themisv1.TargetRef{Group: t.Group, Kind: t.Kind, Namespace: t.Namespace, Name: t.Name, Uid: t.UID},
		ExpectedEffect:   "pods stop being OOMKilled",
		Rationale:        "revision 2 lowered the memory limit to 64Mi",
		CitedEvidenceIds: []string{e.evidence},
		CitedPredicates:  []string{"oom_killed_new_revision"},
		Provenance:       &themisv1.Provenance{Source: "themis-intelligence", Model: "qwen3"},
		Capability:       &themisv1.SubmitProposalRequest_RollbackToRevision{RollbackToRevision: &themisv1.RollbackToRevisionParams{Revision: 1}},
	}
}

// valid is a valid suggestion of a rollback under the lease.
func (e *env) valid(clientID string) suggestion.Suggestion {
	return suggestion.Request(e.request(clientID))
}

// outside is s as a suggester outside Themis makes it, naming the incident.
func (e *env) outside(s suggestion.Suggestion) suggestion.Suggestion {
	s.Lease, s.Incident = "", e.incident
	return s
}

// agent may suggest a rollback in the incident's namespace.
func (e *env) agent() suggestion.Suggester {
	return suggestion.Suggester{Source: "serviceaccount:agents/triage", Namespaces: []string{e.target.Namespace}, Fixes: []string{rollback.Name}, PerMinute: 100}
}

func (e *env) submit(t *testing.T, from suggestion.Suggester, s suggestion.Suggestion) store.Proposal {
	t.Helper()
	p, err := e.inbox.Submit(t.Context(), from, s)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return p
}

func (e *env) setState(t *testing.T, state incident.State) {
	t.Helper()
	if _, err := e.pool.Exec(t.Context(), `UPDATE incidents SET state = $2 WHERE id = $1`, e.incident, state); err != nil {
		t.Fatal(err)
	}
}

func (e *env) state(t *testing.T) incident.State {
	t.Helper()
	in, err := e.store.Incident(t.Context(), e.incident)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

func (e *env) governJobs(t *testing.T, proposalID string) int {
	t.Helper()
	return pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'proposalID' = $2 AND args->>'incidentID' = $3`,
		governance.GovernArgs{}.Kind(), proposalID, e.incident)
}

func generic(params map[string]any) func(*themisv1.SubmitProposalRequest) {
	return func(r *themisv1.SubmitProposalRequest) {
		p, _ := structpb.NewStruct(params)
		r.Capability, r.CapabilityName, r.Params = nil, rollback.Name, p
	}
}

// A suggestion Themis can't act on is kept, rejected with why, and counted.
// One that points at something it shouldn't is a safety event too.
func TestSubmit_Rejected(t *testing.T) {
	agent := func(e *env) suggestion.Suggester { return e.agent() }
	investigator := func(*env) suggestion.Suggester { return suggestion.Investigator }
	for _, tc := range []struct {
		name   string
		from   func(*env) suggestion.Suggester
		change func(*testing.T, *env, *themisv1.SubmitProposalRequest)
		fixes  []string // the profile's; nil allows rollback
		reason themisv1.RejectionReason
		says   string
		safety string // the safety event counted; "" counts none
	}{{
		name: "a fix no profile allows", from: investigator, fixes: []string{},
		reason: themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED, says: "None of this incident's profiles (diagnosis, rollout-regression) allows this kind of fix (deployment.rollback-to-revision).", safety: "unknown_capability",
	}, {
		name: "a fix Themis doesn't have", from: investigator, fixes: []string{"x.unknown"},
		change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) {
			generic(map[string]any{})(r)
			r.CapabilityName = "x.unknown"
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED, says: "(x.unknown)", safety: "unknown_capability",
	}, {
		name: "another target", from: investigator,
		change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) {
			r.Target = &themisv1.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "kube-system", Name: "coredns", Uid: "u-coredns"}
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH, says: "It names Deployment kube-system/coredns (uid u-coredns), but the incident is about shop/checkout (uid u-checkout).", safety: "wrong_target",
	}, {
		// Same name, another UID: the object was recreated.
		name: "the target recreated", from: agent,
		change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) { r.Target.Uid = "u-recreated" },
		reason: themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH, says: "uid u-recreated", safety: "wrong_target",
	}, {
		name: "a revision the older form can't take", from: investigator,
		change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) {
			r.Capability = &themisv1.SubmitProposalRequest_RollbackToRevision{RollbackToRevision: &themisv1.RollbackToRevisionParams{Revision: 0}}
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS, says: "The revision must be a positive number.",
	}, {
		name: "no revision", from: investigator, change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) { generic(map[string]any{})(r) },
		reason: themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS, says: "A rollback needs only revision, a positive whole number.",
	}, {
		name: "a fractional revision", from: investigator, change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) {
			generic(map[string]any{"revision": 1.5})(r)
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS, says: "positive whole number",
	}, {
		name: "an unknown parameter", from: agent, change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) {
			generic(map[string]any{"revision": 1, "force": true})(r)
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS, says: "only revision",
	}, {
		name: "no evidence", from: investigator, change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) { r.CitedEvidenceIds = nil },
		reason: themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE, says: "It cites no evidence.", safety: "unknown_evidence",
	}, {
		name: "evidence never collected", from: agent,
		change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) {
			r.CitedEvidenceIds = []string{"0192f0c1-0000-7000-8000-0000000000ee"}
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE, says: "It cites evidence Themis never collected: 0192f0c1-0000-7000-8000-0000000000ee.", safety: "unknown_evidence",
	}, {
		// A check is cited for what it answered for this incident: one Themis
		// has, but didn't run for it, answered nothing.
		name: "a check the incident didn't run", from: investigator, change: func(_ *testing.T, _ *env, r *themisv1.SubmitProposalRequest) {
			r.CitedPredicates = []string{"rollout_recent"}
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_PREDICATE, says: "It cites a check Themis didn't run for this incident: rollout_recent.",
	}, {
		name: "a fix the source may not suggest",
		from: func(e *env) suggestion.Suggester {
			a := e.agent()
			a.Fixes = []string{"something.else"}
			return a
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED, says: "serviceaccount:agents/triage may not suggest this kind of fix (deployment.rollback-to-revision).", safety: "unknown_capability",
	}, {
		name: "an incident past taking suggestions", from: agent,
		change: func(t *testing.T, e *env, _ *themisv1.SubmitProposalRequest) {
			e.setState(t, incident.ClosedNoAction)
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_INCIDENT_CLOSED, says: "This incident is past the point of taking suggestions.",
	}, {
		name: "a profile no longer configured", from: agent,
		change: func(_ *testing.T, e *env, _ *themisv1.SubmitProposalRequest) { e.inbox.Config = config.Config{} },
		reason: themisv1.RejectionReason_REJECTION_REASON_INCIDENT_CLOSED, says: "Themis no longer has the diagnosis and rollout-regression profiles this incident matched.",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.fixes)
			from := tc.from(e)
			rejected := metrictest.Delta(t, "themis_proposals_total", map[string]string{"result": "rejected"})
			external := metrictest.Delta(t, "themis_external_proposals_total", map[string]string{"result": "rejected"})
			counted := map[string]func() float64{}
			for _, kind := range []string{"unknown_capability", "unknown_evidence", "wrong_target"} {
				counted[kind] = metrictest.Delta(t, "themis_safety_events_total", map[string]string{"kind": kind})
			}
			req := e.request("c1")
			if tc.change != nil {
				tc.change(t, e, req)
			}
			s := suggestion.Request(req)
			if from.Source != "" {
				s = e.outside(s)
			}
			p := e.submit(t, from, s)
			if p.ID == "" || p.Status != store.ProposalRejected || p.RejectionReason != tc.reason.String() || !strings.Contains(p.Message, tc.says) {
				t.Errorf("stored %s %s %q, want rejected with %s saying %q", p.Status, p.RejectionReason, p.Message, tc.reason, tc.says)
			}
			wantSafety := 0
			if tc.reason == themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH {
				wantSafety = 1
			}
			if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM safety_events WHERE proposal_id = $1 AND kind = 'target_mismatch' AND detail = $2`, p.ID, p.Message); n != wantSafety {
				t.Errorf("%d safety events kept, want %d", n, wantSafety)
			}
			if got := rejected(); got != 1 {
				t.Errorf("counted %v rejected proposals, want 1", got)
			}
			if got, want := external(), map[bool]float64{true: 1}[from.Source != ""]; got != want {
				t.Errorf("counted %v rejected from outside, want %v", got, want)
			}
			for kind, delta := range counted {
				want := map[bool]float64{true: 1}[kind == tc.safety]
				if got := delta(); got != want {
					t.Errorf("counted %v %s events, want %v", got, kind, want)
				}
			}
		})
	}
}

// What isn't a suggestion at all, or comes from someone who may not make it,
// is turned away and leaves nothing.
func TestSubmit_Refused(t *testing.T) {
	missing := "00000000-0000-7000-8000-000000000000"
	for _, tc := range []struct {
		name    string
		outside bool
		change  func(*env, *themisv1.SubmitProposalRequest)
		from    func(*env) suggestion.Suggester
		kind    suggestion.Kind
		says    string
	}{
		{
			name: "the investigator naming an incident", change: func(e *env, r *themisv1.SubmitProposalRequest) { r.IncidentId = e.incident },
			kind: suggestion.Unauthenticated, says: "only a caller outside Themis names an incident",
		},
		{name: "no target", change: func(_ *env, r *themisv1.SubmitProposalRequest) { r.Target = nil }, kind: suggestion.Invalid, says: "target is required"},
		{name: "no client ID", change: func(_ *env, r *themisv1.SubmitProposalRequest) { r.ClientProposalId = "" }, kind: suggestion.Invalid, says: "client_proposal_id is required"},
		{
			name: "both forms", change: func(_ *env, r *themisv1.SubmitProposalRequest) { r.CapabilityName = rollback.Name },
			kind: suggestion.Invalid, says: "set capability_name or rollback_to_revision, not both",
		},
		{name: "no fix", change: func(_ *env, r *themisv1.SubmitProposalRequest) { r.Capability = nil }, kind: suggestion.Invalid, says: "a capability is required"},
		{
			name: "a profile no longer configured", change: func(e *env, _ *themisv1.SubmitProposalRequest) { e.inbox.Config = config.Config{} },
			kind: suggestion.Gone, says: `no profile of this incident is configured any more (diagnosis, rollout-regression)`,
		},
		{
			name: "a caller outside naming a lease", outside: true, change: func(e *env, r *themisv1.SubmitProposalRequest) { r.LeaseId = e.lease },
			kind: suggestion.Forbidden, says: "a lease belongs to the investigator",
		},
		{
			name: "a caller outside naming no incident", outside: true, change: func(_ *env, r *themisv1.SubmitProposalRequest) { r.IncidentId = "" },
			kind: suggestion.Invalid, says: "incident_id is required",
		},
		{
			name: "a suggestion too long", outside: true, change: func(_ *env, r *themisv1.SubmitProposalRequest) { r.Rationale = strings.Repeat("x", 5000) },
			kind: suggestion.Invalid, says: "the suggestion is too long",
		},
		{
			name: "a caller outside with no target", outside: true, change: func(_ *env, r *themisv1.SubmitProposalRequest) { r.Target = nil },
			kind: suggestion.Invalid, says: "target is required",
		},
		// Out of scope, an incident looks just like one that doesn't exist.
		{
			name: "an incident that doesn't exist", outside: true, change: func(_ *env, r *themisv1.SubmitProposalRequest) { r.IncidentId = missing },
			kind: suggestion.NotFound, says: "no such incident",
		},
		{name: "an incident out of scope", outside: true, from: func(e *env) suggestion.Suggester {
			a := e.agent()
			a.Namespaces = []string{"elsewhere"}
			return a
		}, kind: suggestion.NotFound, says: "no such incident"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			from := suggestion.Investigator
			req := e.request("c1")
			if tc.outside {
				from = e.agent()
				req.LeaseId, req.IncidentId = "", e.incident
			}
			if tc.from != nil {
				from = tc.from(e)
			}
			if tc.change != nil {
				tc.change(e, req)
			}
			_, err := e.inbox.Submit(t.Context(), from, suggestion.Request(req))
			var refused *suggestion.Error
			if !errors.As(err, &refused) || refused.Kind != tc.kind || refused.Error() != tc.says {
				t.Errorf("err = %#v, want kind %d saying %q", err, tc.kind, tc.says)
			}
			if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM proposals`); n != 0 {
				t.Errorf("%d proposals kept, want none", n)
			}
		})
	}
}

func TestSubmit_LostLease(t *testing.T) {
	e := newEnv(t, nil)
	s := e.valid("c1")
	s.Lease = uuid.NewString()
	if _, err := e.inbox.Submit(t.Context(), suggestion.Investigator, s); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("err = %v, want the lease lost", err)
	}
}

// A source outside Themis is limited in how often it suggests, and in how
// much it leaves on one incident.
func TestSubmit_LimitsASourceOutside(t *testing.T) {
	tooMany := func(t *testing.T, err error, says string) {
		t.Helper()
		var refused *suggestion.Error
		if !errors.As(err, &refused) || refused.Kind != suggestion.TooMany || refused.Message != says {
			t.Errorf("err = %v, want too many saying %q", err, says)
		}
	}
	t.Run("how often", func(t *testing.T) {
		e := newEnv(t, nil)
		limited := metrictest.Delta(t, "themis_requests_limited_total", map[string]string{"kind": "suggestions"})
		agent := e.agent()
		agent.PerMinute = 1
		e.submit(t, agent, e.outside(e.valid("c1")))
		_, err := e.inbox.Submit(t.Context(), agent, e.outside(e.valid("c2")))
		tooMany(t, err, "too many suggestions: wait a minute and try again")
		if got := limited(); got != 1 {
			t.Errorf("counted %v limited suggestions, want 1", got)
		}
	})
	t.Run("how many", func(t *testing.T) {
		e := newEnv(t, nil)
		for i := range 20 {
			e.submit(t, e.agent(), e.outside(e.valid(string(rune('a'+i)))))
		}
		_, err := e.inbox.Submit(t.Context(), e.agent(), e.outside(e.valid("one-too-many")))
		tooMany(t, err, "this source has suggested too many fixes for this incident")
	})
}

// A suggestion is stored with the version of the fix it names.
func TestSubmit_StoresTheFixVersion(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request("c1")
	generic(map[string]any{"pods": "all"})(req)
	req.CapabilityName = "test.restart"
	p := e.submit(t, suggestion.Investigator, suggestion.Request(req))
	if p.Status != store.ProposalAccepted || p.Capability != "test.restart" || p.CapabilityVersion != "7" {
		t.Errorf("stored %s %s v%q, want test.restart accepted at its version 7", p.Status, p.Capability, p.CapabilityVersion)
	}
}

// The generic form stores exactly what the older rollback form does, so
// everything after it, the approval's digest included, is the same.
func TestSubmit_GenericFormStoresWhatTheOlderFormDoes(t *testing.T) {
	e := newEnv(t, nil)
	older := e.submit(t, suggestion.Investigator, e.valid("older"))
	req := e.request("generic")
	generic(map[string]any{"revision": 1})(req)
	got := e.submit(t, suggestion.Investigator, suggestion.Request(req))
	if got.Status != store.ProposalAccepted || got.Capability != older.Capability || got.CapabilityVersion != older.CapabilityVersion || !reflect.DeepEqual(got.Params, older.Params) {
		t.Errorf("stored %s v%s %#v; the older form stores %s v%s %#v",
			got.Capability, got.CapabilityVersion, got.Params, older.Capability, older.CapabilityVersion, older.Params)
	}
}

// Who suggested a fix is who Themis proved it is. What it claims is kept apart.
func TestSubmit_RecordsWhoSuggested(t *testing.T) {
	e := newEnv(t, nil)
	own := e.submit(t, suggestion.Investigator, e.valid("c1"))
	if own.LeaseID != e.lease || own.Provenance["source"] != "themis-intelligence" || !own.Authenticated {
		t.Errorf("the investigator's = %+v", own)
	}
	for _, person := range []string{"", "alex"} {
		source := map[string]string{"": suggestion.PersonSource, "alex": "person:alex"}[person]
		p := e.submit(t, suggestion.Person(person), e.outside(e.valid(source)))
		if p.LeaseID != "" || p.Provenance["source"] != source || p.Provenance["claimedSource"] != "themis-intelligence" || !p.Authenticated || p.Status != store.ProposalAccepted {
			t.Errorf("%s's = %+v", source, p)
		}
	}
}

// What the suggester looked at is kept for people, and bounded.
func TestSubmit_KeepsNotesBounded(t *testing.T) {
	e := newEnv(t, nil)
	s := e.valid("c1")
	s.Notes = "ran: kubectl get pods\n" + strings.Repeat("é", 5000)
	p := e.submit(t, suggestion.Investigator, s)
	if !strings.HasPrefix(p.Notes, "ran: kubectl get pods") || len(p.Notes) > 8<<10+16 || !utf8.ValidString(p.Notes) || !strings.HasSuffix(p.Notes, "(cut)") {
		t.Errorf("notes = %d bytes, valid %v", len(p.Notes), utf8.ValidString(p.Notes))
	}
}

// A retry returns the stored suggestion and never changes it, even when it
// says something else, and is counted once; a new client ID is a new one.
func TestSubmit_RetryStoresNothingNew(t *testing.T) {
	for name, outside := range map[string]bool{"the investigator": false, "a caller outside": true} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			from, s := suggestion.Investigator, e.valid("c1")
			if outside {
				from, s = e.agent(), e.outside(s)
			}
			accepted := metrictest.Delta(t, "themis_proposals_total", map[string]string{"result": "accepted"})
			first := e.submit(t, from, s)
			s.Rationale = "a changed mind"
			if again := e.submit(t, from, s); again.ID != first.ID || again.Rationale != first.Rationale {
				t.Errorf("a retry stored %+v, want %+v", again, first)
			}
			if got := accepted(); got != 1 {
				t.Errorf("counted %v accepted, want 1", got)
			}
			s.ClientID = "c2"
			if other := e.submit(t, from, s); other.ID == first.ID {
				t.Error("a new client ID did not store a new suggestion")
			}
		})
	}
}

// An accepted suggestion from outside goes to governance at once when the
// incident waits for one. While the investigation runs, nothing moves: it
// picks one up when it completes.
func TestSubmit_HandsOverWhenTheIncidentWaits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   incident.State
		outside bool
		handed  bool
	}{
		{"from outside, after Themis couldn't decide", incident.Inconclusive, true, true},
		{"from outside, without an investigator", incident.AwaitingSuggestion, true, true},
		{"from outside, during the investigation", incident.Investigating, true, false},
		{"from the investigator", incident.Investigating, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.setState(t, tc.state)
			from, s := suggestion.Investigator, e.valid("c1")
			if tc.outside {
				from, s = e.agent(), e.outside(s)
			}
			p := e.submit(t, from, s)
			want, jobs := tc.state, 0
			if tc.handed {
				want, jobs = incident.Diagnosed, 1
			}
			if got := e.state(t); got != want {
				t.Errorf("state = %s, want %s", got, want)
			}
			if n := e.governJobs(t, p.ID); n != jobs {
				t.Errorf("%d govern jobs, want %d", n, jobs)
			}
		})
	}
}

// HandOver governs the investigation's own accepted suggestion first, then
// the first from outside; without a lease, only those from outside.
func TestHandOver(t *testing.T) {
	for _, tc := range []struct {
		name    string
		own     bool // the investigation suggested a fix
		outside bool // a caller outside did
		lease   bool // HandOver is given the lease
		want    string
	}{
		{"its own first", true, true, true, "own"},
		{"from outside when it has none", false, true, true, "outside"},
		{"only from outside without the lease", true, false, false, ""},
		{"nothing accepted", false, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			ids := map[string]string{}
			if tc.outside {
				ids["outside"] = e.submit(t, e.agent(), e.outside(e.valid("theirs"))).ID
			}
			if tc.own {
				ids["own"] = e.submit(t, suggestion.Investigator, e.valid("ours")).ID
			}
			// A rejected suggestion is never governed.
			bad := e.valid("bad")
			bad.Evidence = nil
			e.submit(t, suggestion.Investigator, bad)
			lease := ""
			if tc.lease {
				lease = e.lease
			}

			var handed bool
			err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
				var err error
				handed, err = e.inbox.HandOver(t.Context(), tx, e.incident, lease, "found it", incident.Investigating)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if handed != (tc.want != "") {
				t.Fatalf("handed = %v, want %v", handed, tc.want != "")
			}
			if tc.want == "" {
				if got := e.state(t); got != incident.Investigating {
					t.Errorf("state = %s, want investigating", got)
				}
				return
			}
			if n := e.governJobs(t, ids[tc.want]); n != 1 {
				t.Errorf("%d govern jobs for the %s suggestion, want 1", n, tc.want)
			}
			trail, err := e.store.Transitions(t.Context(), e.incident)
			if err != nil {
				t.Fatal(err)
			}
			if last := trail[len(trail)-1]; last.To != incident.Diagnosed || last.Reason != "found it" {
				t.Errorf("last transition = %+v", last)
			}
		})
	}
}
