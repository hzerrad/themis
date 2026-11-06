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
}

func viewSummary(s store.IncidentSummary, causes []catalog.RootCause) *summaryView {
	v := &summaryView{Reason: s.Reason, From: string(s.From)}
	if s.Cause != "" {
		v.Cause = &causeView{Name: s.Cause}
		for _, c := range causes {
			if c.Name == s.Cause {
				v.Cause.Description = c.Description
			}
		}
	}
	if s.Capability != "" {
		v.Fix = &fixView{Capability: s.Capability, Params: s.Params}
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
	// Checks describe each check by name, in plain words.
	Checks map[string]string
	// Causes describe each root cause the investigation may name, by name,
	// in its pack's words.
	Causes        map[string]string
	Investigation *store.InvestigationRecord `json:",omitempty"`
	Proposals     []store.Proposal
	Actions       []actionView

	// Headline opens an investigation that couldn't decide: whether Themis
	// couldn't see enough, or saw it all and found no clear answer.
	Headline string `json:",omitempty"`
	// Missing says, in plain words, what Themis couldn't see, why, and how
	// to let it see next time.
	Missing []string `json:",omitempty"`
	// EvidenceWarning says that some evidence looks like instructions or
	// forged tool output, for whoever approves a fix.
	EvidenceWarning string `json:",omitempty"`
}

func viewAction(a store.Action, fixes capability.Registry) actionView {
	return actionView{
		ID: a.ID, IncidentID: a.IncidentID, ProposalID: a.ProposalID,
		Capability: a.Capability, CapabilityVersion: a.CapabilityVersion,
		Target: a.Target, Params: a.Params, BlastRadius: a.BlastRadius, Risk: a.Risk,
		Decision: a.Decision, DecisionReason: a.DecisionReason, PolicyID: a.PolicyID, PolicyDigest: a.PolicyDigest,
		Digest: a.Digest, State: a.State, ExpiresAt: a.ExpiresAt, CreatedAt: a.CreatedAt,
		Changes: changes(a, fixes),
	}
}
