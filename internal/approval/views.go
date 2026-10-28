package approval

import (
	"encoding/json/jsontext"
	"time"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/predicate"
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

type actionView struct {
	store.Action
	PreState     jsontext.Value
	Desired      jsontext.Value
	Execution    *store.Execution  `json:",omitempty"`
	Verification *verificationView `json:",omitempty"`
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
