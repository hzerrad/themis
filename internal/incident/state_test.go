package incident_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/incident"
)

var allStates = []incident.State{
	incident.Received,
	incident.Investigating,
	incident.AwaitingSuggestion,
	incident.Diagnosed,
	incident.Inconclusive,
	incident.Recommended,
	incident.Proposed,
	incident.AwaitingApproval,
	incident.Authorized,
	incident.Executing,
	incident.Verifying,
	incident.Restoring,
	incident.Resolved,
	incident.ContainedEscalated,
	incident.Escalated,
	incident.ClosedNoAction,
	incident.SelfResolved,
	incident.Rejected,
	incident.Canceled,
}

func successors(from incident.State) []incident.State {
	var out []incident.State
	for _, to := range allStates {
		if incident.CanTransition(from, to) {
			out = append(out, to)
		}
	}
	return out
}

func TestTerminalStates(t *testing.T) {
	want := map[incident.State]bool{
		incident.Resolved:           true,
		incident.ContainedEscalated: true,
		incident.Escalated:          true,
		incident.ClosedNoAction:     true,
		incident.SelfResolved:       true,
		incident.Rejected:           true,
		incident.Canceled:           true,
	}
	for _, s := range allStates {
		if got := s.Terminal(); got != want[s] {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want[s])
		}
	}
	if got := len(incident.TerminalStates()); got != len(want) {
		t.Errorf("len(TerminalStates()) = %d, want %d", got, len(want))
	}
	incident.TerminalStates()[0] = incident.Received
	if incident.Received.Terminal() {
		t.Error("mutating the result of TerminalStates() changed Terminal()")
	}
}

func TestCanTransition(t *testing.T) {
	tests := []struct {
		from, to incident.State
		want     bool
	}{
		// The happy path.
		{incident.Received, incident.Investigating, true},
		{incident.Investigating, incident.Diagnosed, true},
		{incident.Diagnosed, incident.Proposed, true},
		{incident.Proposed, incident.AwaitingApproval, true},
		{incident.AwaitingApproval, incident.Authorized, true},
		{incident.Authorized, incident.Executing, true},
		{incident.Executing, incident.Verifying, true},
		{incident.Verifying, incident.Resolved, true},
		// Nothing skips governance.
		{incident.Proposed, incident.Executing, false},
		{incident.Diagnosed, incident.Authorized, false},
		{incident.Diagnosed, incident.Executing, false},
		{incident.AwaitingApproval, incident.Executing, false},
		{incident.Investigating, incident.Proposed, false},
		{incident.Received, incident.Executing, false},
		// Side exits.
		{incident.AwaitingApproval, incident.AwaitingSuggestion, true}, // denied: another fix may come
		{incident.AwaitingApproval, incident.ClosedNoAction, true},     // denied and closed
		{incident.AwaitingApproval, incident.Recommended, false},       // only a decision recommends
		{incident.AwaitingApproval, incident.Escalated, false},         // approval expired: it waits for a person's fix
		{incident.AwaitingApproval, incident.Diagnosed, false},         // one decision at a time
		{incident.Proposed, incident.Authorized, true},                 // policy allows without approval
		{incident.Received, incident.Rejected, true},                   // out of scope
		{incident.AwaitingApproval, incident.SelfResolved, true},
		{incident.Authorized, incident.SelfResolved, false}, // action in flight
		{incident.Executing, incident.SelfResolved, false},
		{incident.Verifying, incident.SelfResolved, false},
		{incident.Authorized, incident.Canceled, true},
		{incident.Executing, incident.Canceled, false}, // write may have happened
		{incident.Verifying, incident.Canceled, false},
		{incident.Inconclusive, incident.Diagnosed, true}, // someone suggested a fix
		{incident.Inconclusive, incident.Authorized, false},
		{incident.Received, incident.AwaitingSuggestion, true},  // no investigator installed
		{incident.AwaitingSuggestion, incident.Diagnosed, true}, // someone suggested a fix
		{incident.AwaitingSuggestion, incident.ClosedNoAction, true},
		{incident.AwaitingSuggestion, incident.Authorized, false},
		{incident.AwaitingSuggestion, incident.Investigating, true}, // a person asked to investigate again
		{incident.AwaitingSuggestion, incident.AwaitingApproval, false},
		{incident.Investigating, incident.AwaitingSuggestion, true}, // found nothing a person can use
		{incident.Diagnosed, incident.AwaitingSuggestion, true},     // a person's fix refused
		{incident.Proposed, incident.AwaitingSuggestion, true},      // a person's fix refused by policy
		{incident.Inconclusive, incident.AwaitingSuggestion, false},
		// A fix that didn't help is put back, and the incident then goes to a
		// person. While Themis is putting it back, a write is in flight:
		// nothing cancels it.
		{incident.Verifying, incident.Restoring, true},
		{incident.Restoring, incident.Escalated, true},
		{incident.Restoring, incident.Canceled, false},
		{incident.Restoring, incident.SelfResolved, false},
		{incident.Restoring, incident.Resolved, false},
		{incident.Restoring, incident.Verifying, false},
		// Unknown states.
		{"bogus", incident.Investigating, false},
		{incident.Received, "bogus", false},
	}
	for _, tt := range tests {
		if got := incident.CanTransition(tt.from, tt.to); got != tt.want {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

// Terminal states have no way out, every other state has one, and no
// transition leaves a state as it was.
func TestGraph_TerminalIffNoOutgoingEdges(t *testing.T) {
	for _, s := range allStates {
		hasOut := len(successors(s)) > 0
		if s.Terminal() == hasOut {
			t.Errorf("%s: Terminal() = %v but has outgoing edges = %v", s, s.Terminal(), hasOut)
		}
		if incident.CanTransition(s, s) {
			t.Errorf("%s -> %s is allowed; a transition must change state", s, s)
		}
	}
}

func TestGraph_EveryStateReachableFromReceived(t *testing.T) {
	seen := map[incident.State]bool{incident.Received: true}
	queue := []incident.State{incident.Received}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, next := range successors(s) {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	for _, s := range allStates {
		if !seen[s] {
			t.Errorf("%s is unreachable from %s", s, incident.Received)
		}
	}
}

func TestGraph_EveryStateCanReachTerminal(t *testing.T) {
	for _, start := range allStates {
		seen := map[incident.State]bool{start: true}
		queue := []incident.State{start}
		found := false
		for len(queue) > 0 && !found {
			s := queue[0]
			queue = queue[1:]
			if s.Terminal() {
				found = true
				break
			}
			for _, next := range successors(s) {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
		if !found {
			t.Errorf("%s cannot reach a terminal state", start)
		}
	}
}

// States must list every state the graph knows, once.
func TestStates_ListsEveryState(t *testing.T) {
	listed := map[incident.State]bool{}
	for _, s := range incident.States() {
		if listed[s] {
			t.Errorf("%s listed twice", s)
		}
		listed[s] = true
	}
	for _, s := range incident.TerminalStates() {
		if !listed[s] {
			t.Errorf("terminal state %s not listed", s)
		}
	}
	for _, from := range incident.States() {
		for _, to := range incident.States() {
			if incident.CanTransition(from, to) && !listed[to] {
				t.Errorf("%s->%s leads to an unlisted state", from, to)
			}
		}
	}
	if n := len(incident.States()); n != 19 {
		t.Errorf("%d states, want 19: update States when adding one", n)
	}
}
