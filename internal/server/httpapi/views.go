package httpapi

import (
	"encoding/json/jsontext"
	"time"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/store"
)

type incidentView struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Profile string `json:"profile"`
	// Profiles are every profile the incident matched, Profile first.
	Profiles        []string  `json:"profiles"`
	Target          any       `json:"target"`
	OccurrenceCount int       `json:"occurrenceCount"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	SupersededBy    string    `json:"supersededBy,omitempty"`
	// Summary says in brief what happened and what Themis proposes. Only
	// the list carries it: the detail has the whole record.
	Summary *summaryView `json:"summary,omitempty"`
}

// summaryView is an incident in brief, for a list of incidents.
type summaryView struct {
	// Cause is the root cause the investigation leads with, by name and in
	// its pack's words.
	Cause *causeView `json:"cause,omitempty"`
	// Fix is the latest fix for the incident.
	Fix *fixView `json:"fix,omitempty"`
	// Rule is the policy rule that decided the latest change, such as
	// gitops-owns-the-target for one Themis only recommends.
	Rule string `json:"rule,omitempty"`
	// Reason is why the incident last changed state, and From the state it
	// left.
	Reason string `json:"reason,omitempty"`
	From   string `json:"from,omitempty"`
}

type causeView struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type fixView struct {
	Capability string         `json:"capability"`
	Params     map[string]any `json:"params"`
	// Words name the change, as its fix words it.
	Words capability.Words `json:"words"`
}

func viewSummary(s store.IncidentSummary, target string, causes []catalog.RootCause, fixes capability.Registry) *summaryView {
	v := &summaryView{Reason: s.Reason, From: string(s.From), Rule: s.Rule}
	if s.Cause != "" {
		v.Cause = &causeView{Name: s.Cause}
		for _, c := range causes {
			if c.Name == s.Cause {
				v.Cause.Description = c.Description
			}
		}
	}
	if s.Capability != "" {
		v.Fix = &fixView{Capability: s.Capability, Params: s.Params, Words: fixes.Words(s.Capability, target, s.Params, s.PreState, s.Desired)}
	}
	return v
}

func viewIncident(in store.Incident) incidentView {
	return incidentView{
		ID: in.ID, State: string(in.State), Profile: in.Profile, Profiles: in.Profiles(), Target: in.Target,
		OccurrenceCount: in.OccurrenceCount, CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt,
		SupersededBy: in.SupersededBy,
	}
}

// evidenceView serves Data as JSON rather than base64.
type evidenceView struct {
	evidence.Item
	// Data is left out for people who may not see raw evidence.
	Data jsontext.Value `json:",omitzero"`
	// Suspicious says, for each flagged text, what in the item holds it and
	// what it is, in plain words.
	Suspicious []suspicionView `json:",omitempty"`
	// Number is the item's place in the list, from 1, by which facts and
	// findings cite it, and Kind names its template, such as "Pod states".
	Number int
	Kind   string
	// Later is set for an item collected well after the first.
	Later bool `json:",omitempty"`
	// Digest says what the item holds in one line, and View lays it out by
	// its kind. Both are left out for people who may not see raw evidence.
	Digest phrase    `json:",omitempty"`
	View   *readable `json:",omitzero"`
}

// suspicionView is text flagged in an item, as people read it.
type suspicionView struct {
	Reason string `json:"reason"`
	At     string `json:"at"`
	// Excerpt is left out for people who may not see raw evidence.
	Excerpt string `json:"excerpt,omitempty"`
	// Origin names what holds the text, such as a pod and container.
	Origin string `json:"origin,omitempty"`
	// What says what the text is, such as "is written as an instruction to
	// Themis".
	What string `json:"what"`
}

// actionView lists its fields rather than embedding store.Action: the
// templates hold env values and must never be served.
type actionView struct {
	ID                string
	IncidentID        string
	ProposalID        string
	Capability        string
	CapabilityVersion string
	Target            digest.TargetIdentity
	Params            map[string]any
	BlastRadius       map[string]any
	Risk              string
	Decision          string
	DecisionReason    string
	PolicyID          string
	PolicyDigest      string
	Digest            string
	State             store.ActionState
	ExpiresAt         *time.Time
	CreatedAt         time.Time
	Changes           []capability.Change `json:",omitempty"`
	Execution         *store.Execution    `json:",omitempty"`
	Verification      *verificationView   `json:",omitempty"`
	// Words name the change, as its fix words it.
	Words capability.Words
}

// proposalView is a suggested fix, with the change it asks for named.
type proposalView struct {
	store.Proposal
	// Words name the change, from its parameters alone: it may never have
	// been worked out against the cluster.
	Words capability.Words
}

type verificationView struct {
	store.Verification
	Observation jsontext.Value
}

// detailView puts the proposal next to the action derived from it.
type detailView struct {
	Incident    incidentView
	Transitions []store.Transition
	Evidence    []evidenceView
	Predicates  []check.Result
	// Facts are what the checks found that matters most, as sentences that
	// cite their evidence. Situation is what fails, in a few words, from
	// the symptoms they found.
	Facts     []factView `json:",omitempty"`
	Situation phrase     `json:",omitempty"`
	// Symptoms are the checks that find the incident's failure.
	Symptoms []string `json:",omitempty"`
	// Checks say in plain words what each check, by name, finds.
	Checks map[string]checkWords
	// Causes describe each root cause the investigation may name, by name,
	// in its pack's words.
	Causes        map[string]string
	Investigation *store.InvestigationRecord `json:",omitempty"`
	Proposals     []proposalView
	Actions       []actionView
	// Basis says, by name, what each check that came out true and only
	// says whether figures are there rests on, such as "Prometheus's
	// container CPU history over 1h".
	Basis map[string]string `json:",omitempty"`

	// Headline opens an investigation that couldn't decide: whether Themis
	// couldn't see enough, or saw it all and found no clear answer.
	Headline string `json:",omitempty"`
	// Missing says, in plain words, what Themis couldn't see, why, and how
	// to let it see next time.
	Missing []string `json:",omitempty"`
	// EvidenceWarning says that some evidence looks like instructions or
	// forged tool output, for whoever approves a fix.
	EvidenceWarning string `json:",omitempty"`
	// Fixes are the fixes the incident's profiles offer, each once, primary
	// profile first: what a person may suggest for it.
	Fixes []offeredFix `json:",omitempty"`

	// Follows is set when the incident opened on a workload that runs a
	// fix Themis applied for an earlier incident.
	Follows *followsView `json:",omitzero"`

	// InvestigateAgain is set while a person can have the incident
	// investigated again, and says why it can't be, if it can't.
	InvestigateAgain *againView `json:",omitzero"`
	// AnotherApprover is set when a fix needs an approver other than the
	// person who suggested it.
	AnotherApprover bool `json:",omitempty"`
}

// followsView is Themis's fix the workload ran when the incident opened,
// from the evidence Themis keeps of it.
type followsView struct {
	// Incident is the earlier incident whose fix it was.
	Incident string `json:"incident"`
	// Done says what the fix did, such as "Set the CPU limit of app from
	// 200m to 300m."
	Done      string    `json:"done"`
	AppliedAt time.Time `json:"appliedAt"`
	// Before is how long before this incident opened it was applied, in
	// seconds.
	Before int64 `json:"before"`
}

// againView is whether investigating an incident again is available.
type againView struct {
	// Refused says why it isn't, such as a token budget spent.
	Refused string `json:"refused,omitempty"`
}

// offeredFix is a fix as a person suggests it: what it does, and the JSON
// Schema of its parameters.
type offeredFix struct {
	Name string `json:"name"`
	// Noun is the fix in a few plain words, such as "a rollback".
	Noun        string         `json:"noun"`
	Description string         `json:"description"`
	Schema      jsontext.Value `json:"schema"`
}

// checkWords are what a check finds in each answer, in its pack's words.
type checkWords struct {
	// True is what it finds when true.
	True string `json:"true"`
	// False is what it finds when false; empty when the pack doesn't say.
	False string `json:"false,omitempty"`
	// Topic names what it is about, for saying it is unknown.
	Topic string `json:"topic,omitempty"`
	// Reads are the evidence templates it reads.
	Reads []string `json:"reads,omitempty"`
	// Availability is set for a check that says only whether figures other
	// checks need are there: people read it as what they rest on.
	Availability bool `json:"availability,omitempty"`
}

func viewAction(a store.Action, fixes capability.Registry) actionView {
	return actionView{
		ID: a.ID, IncidentID: a.IncidentID, ProposalID: a.ProposalID,
		Capability: a.Capability, CapabilityVersion: a.CapabilityVersion,
		Target: a.Target, Params: a.Params, BlastRadius: a.BlastRadius, Risk: a.Risk,
		Decision: a.Decision, DecisionReason: a.DecisionReason, PolicyID: a.PolicyID, PolicyDigest: a.PolicyDigest,
		Digest: a.Digest, State: a.State, ExpiresAt: a.ExpiresAt, CreatedAt: a.CreatedAt,
		Changes: changes(a, fixes), Words: fixes.Words(a.Capability, a.Target.Name, a.Params, a.PreState, a.Desired),
	}
}
