// Package governance turns accepted proposals into governed actions.
package governance

// GovernArgs govern an accepted proposal.
type GovernArgs struct {
	ProposalID string `json:"proposalID"`
	IncidentID string `json:"incidentID"`
}

// Kind is persisted with each job and must never change.
func (GovernArgs) Kind() string { return "govern" }

func (a GovernArgs) Incident() string { return a.IncidentID }

// ExecuteArgs execute an authorized action.
type ExecuteArgs struct {
	ActionID   string `json:"actionID"`
	IncidentID string `json:"incidentID"`
}

// Kind is persisted with each job and must never change.
func (ExecuteArgs) Kind() string { return "execute" }

func (a ExecuteArgs) Incident() string { return a.IncidentID }
