package approval

import (
	"encoding/json/jsontext"
	"time"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
)

type incidentView struct {
	ID              string    `json:"id"`
	State           string    `json:"state"`
	Profile         string    `json:"profile"`
	Target          any       `json:"target"`
	OccurrenceCount int       `json:"occurrenceCount"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func viewIncident(in store.Incident) incidentView {
	return incidentView{
		ID: in.ID, State: string(in.State), Profile: in.Profile, Target: in.Target,
		OccurrenceCount: in.OccurrenceCount, CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt,
	}
}

// evidenceView serves Data as JSON rather than base64.
type evidenceView struct {
	evidence.Item
	Data jsontext.Value
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
	Changes           []rollback.Change `json:",omitempty"`
	Execution         *store.Execution  `json:",omitempty"`
	Verification      *verificationView `json:",omitempty"`
}

type verificationView struct {
	store.Verification
	Observation jsontext.Value
}

// detailView puts the proposal next to the action derived from it.
type detailView struct {
	Incident      incidentView
	Transitions   []store.Transition
	Evidence      []evidenceView
	Predicates    []predicate.Result
	Investigation *store.InvestigationRecord `json:",omitempty"`
	Proposals     []store.Proposal
	Actions       []actionView
}

func viewAction(a store.Action) actionView {
	return actionView{
		ID: a.ID, IncidentID: a.IncidentID, ProposalID: a.ProposalID,
		Capability: a.Capability, CapabilityVersion: a.CapabilityVersion,
		Target: a.Target, Params: a.Params, BlastRadius: a.BlastRadius, Risk: a.Risk,
		Decision: a.Decision, DecisionReason: a.DecisionReason, PolicyID: a.PolicyID, PolicyDigest: a.PolicyDigest,
		Digest: a.Digest, State: a.State, ExpiresAt: a.ExpiresAt, CreatedAt: a.CreatedAt,
		Changes: changes(a),
	}
}
