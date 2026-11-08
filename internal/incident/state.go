package incident

import (
	"slices"
)

// State is the lifecycle state of an incident. Values are persisted, so they
// must never change once released.
type State string

const (
	Received      State = "received"
	Investigating State = "investigating"
	// AwaitingSuggestion means the incident waits for a fix suggested by a
	// person or a tool outside Themis: no investigator is installed; a person
	// works on it, and denied its fix or had theirs refused; or Themis
	// referred it to a person with no fix applied, while its checks find the
	// failure. A person may also have it investigated again.
	AwaitingSuggestion State = "awaiting_suggestion"
	Diagnosed          State = "diagnosed"
	Inconclusive       State = "inconclusive"
	// Recommended means Themis can't apply the fix policy allows: Argo CD or
	// Flux manages the workload, or the executor may not apply it there. A
	// person makes the change, and Themis watches the workload for it.
	Recommended      State = "recommended"
	Proposed         State = "proposed"
	AwaitingApproval State = "awaiting_approval"
	Authorized       State = "authorized"
	Executing        State = "executing"
	Verifying        State = "verifying"
	// Restoring puts back what Themis changed, after a fix that didn't help.
	Restoring State = "restoring"

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
	Received:           {Investigating, AwaitingSuggestion, Rejected, SelfResolved, Canceled},
	AwaitingSuggestion: {Diagnosed, Investigating, ClosedNoAction, SelfResolved, Canceled},
	Investigating:      {Diagnosed, Inconclusive, AwaitingSuggestion, Escalated, SelfResolved, Canceled},
	Diagnosed:          {Proposed, AwaitingSuggestion, ClosedNoAction, SelfResolved, Canceled},
	Inconclusive:       {Diagnosed, ClosedNoAction, SelfResolved, Canceled},
	Recommended:        {ClosedNoAction, SelfResolved, Canceled},
	Proposed:           {AwaitingApproval, Authorized, Recommended, AwaitingSuggestion, ClosedNoAction, SelfResolved, Canceled},
	AwaitingApproval:   {Authorized, AwaitingSuggestion, ClosedNoAction, SelfResolved, Canceled},
	Authorized:         {Executing, Canceled},
	Executing:          {Verifying, Escalated},
	Verifying:          {Resolved, ContainedEscalated, Escalated, Restoring},
	Restoring:          {Escalated},
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

// States returns every state.
func States() []State {
	return []State{
		Received, Investigating, AwaitingSuggestion, Diagnosed, Inconclusive, Recommended, Proposed, AwaitingApproval, Authorized, Executing, Verifying, Restoring,
		Resolved, ContainedEscalated, Escalated, ClosedNoAction, SelfResolved, Rejected, Canceled,
	}
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
