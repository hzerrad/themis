package incident_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/incident"
)

var allStates = []incident.State{
	incident.Received,
	incident.Investigating,
	incident.Diagnosed,
	incident.Inconclusive,
	incident.Recommended,
	incident.Proposed,
	incident.AwaitingApproval,
	incident.Authorized,
	incident.Executing,
	incident.Verifying,
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
}

func TestTerminalStatesReturnsCopy(t *testing.T) {
	incident.TerminalStates()[0] = incident.Received
	if incident.Received.Terminal() {
		t.Fatal("mutating the result of TerminalStates() changed Terminal()")
	}
}

func TestCanTransition_RejectsSkippingGovernance(t *testing.T) {
	skips := [][2]incident.State{
		{incident.Proposed, incident.Executing},
		{incident.Diagnosed, incident.Authorized},
		{incident.Diagnosed, incident.Executing},
		{incident.AwaitingApproval, incident.Executing},
		{incident.Investigating, incident.Proposed},
		{incident.Received, incident.Executing},
	}
	for _, p := range skips {
		if incident.CanTransition(p[0], p[1]) {
			t.Errorf("CanTransition(%s, %s) = true, want false", p[0], p[1])
		}
	}
}

func TestCanTransition_HappyPath(t *testing.T) {
	path := []incident.State{
		incident.Received,
		incident.Investigating,
		incident.Diagnosed,
		incident.Proposed,
		incident.AwaitingApproval,
		incident.Authorized,
		incident.Executing,
		incident.Verifying,
		incident.Resolved,
	}
	for i := 1; i < len(path); i++ {
		if !incident.CanTransition(path[i-1], path[i]) {
			t.Errorf("CanTransition(%s, %s) = false, want true", path[i-1], path[i])
		}
	}
}

func TestCanTransition_SideExits(t *testing.T) {
	tests := []struct {
		from, to incident.State
		want     bool
	}{
		{incident.AwaitingApproval, incident.Recommended, true}, // denied
		{incident.AwaitingApproval, incident.Escalated, true},   // approval expired
		{incident.Proposed, incident.Authorized, true},          // policy allows without approval
		{incident.Received, incident.Rejected, true},            // out of scope
		{incident.AwaitingApproval, incident.SelfResolved, true},
		{incident.Authorized, incident.SelfResolved, false}, // action in flight
		{incident.Executing, incident.SelfResolved, false},
		{incident.Verifying, incident.SelfResolved, false},
		{incident.Authorized, incident.Canceled, true},
		{incident.Executing, incident.Canceled, false}, // write may have happened
		{incident.Verifying, incident.Canceled, false},
	}
	for _, tt := range tests {
		if got := incident.CanTransition(tt.from, tt.to); got != tt.want {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestCanTransition_UnknownStates(t *testing.T) {
	if incident.CanTransition("bogus", incident.Investigating) {
		t.Error("transition from an unknown state was allowed")
	}
	if incident.CanTransition(incident.Received, "bogus") {
		t.Error("transition to an unknown state was allowed")
	}
}

func TestGraph_TerminalIffNoOutgoingEdges(t *testing.T) {
	for _, s := range allStates {
		hasOut := len(successors(s)) > 0
		if s.Terminal() == hasOut {
			t.Errorf("%s: Terminal() = %v but has outgoing edges = %v", s, s.Terminal(), hasOut)
		}
	}
}

func TestGraph_NoSelfLoops(t *testing.T) {
	for _, s := range allStates {
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
	if n := len(incident.States()); n != 17 {
		t.Errorf("%d states, want 17: update States when adding one", n)
	}
}
