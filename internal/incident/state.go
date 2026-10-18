package incident

import (
	"slices"
)

// State is the lifecycle state of an incident. Values are persisted, so they
// must never change once released.
type State string

const (
	Received         State = "received"
	Investigating    State = "investigating"
	Diagnosed        State = "diagnosed"
	Inconclusive     State = "inconclusive"
	Recommended      State = "recommended"
	Proposed         State = "proposed"
	AwaitingApproval State = "awaiting_approval"
	Authorized       State = "authorized"
	Executing        State = "executing"
	Verifying        State = "verifying"

	Resolved           State = "resolved"
	ContainedEscalated State = "contained_escalated"
	Escalated          State = "escalated"
	ClosedNoAction     State = "closed_no_action"
	SelfResolved       State = "self_resolved"
	Rejected           State = "rejected"
	Canceled           State = "canceled"
)

// transitions is the allowlist of legal state changes.
var transitions = map[State][]State{
	Received:         {Investigating, Rejected, SelfResolved, Canceled},
	Investigating:    {Diagnosed, Inconclusive, Escalated, SelfResolved, Canceled},
	Diagnosed:        {Proposed, Recommended, ClosedNoAction, SelfResolved, Canceled},
	Inconclusive:     {ClosedNoAction, SelfResolved, Canceled},
	Recommended:      {ClosedNoAction, SelfResolved, Canceled},
	Proposed:         {AwaitingApproval, Authorized, Recommended, SelfResolved, Canceled},
	AwaitingApproval: {Authorized, Recommended, Escalated, SelfResolved, Canceled},
	Authorized:       {Executing, Canceled},
	Executing:        {Verifying, Escalated},
	Verifying:        {Resolved, ContainedEscalated, Escalated},
}

var terminal = []State{
	Resolved,
	ContainedEscalated,
	Escalated,
	ClosedNoAction,
	SelfResolved,
	Rejected,
	Canceled,
}

// Terminal reports whether s ends the incident.
func (s State) Terminal() bool {
	return slices.Contains(terminal, s)
}

// TerminalStates returns the terminal states.
func TerminalStates() []State {
	return slices.Clone(terminal)
}

// CanTransition reports whether moving from one state to another is allowed.
func CanTransition(from, to State) bool {
	return slices.Contains(transitions[from], to)
}
