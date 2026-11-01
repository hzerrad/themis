package verify

import (
	"fmt"
	"time"
)

type Outcome string

const (
	Resolved     Outcome = "resolved"
	Contained    Outcome = "contained"
	Unchanged    Outcome = "unchanged"
	Worsened     Outcome = "worsened"
	Inconclusive Outcome = "inconclusive"
)

// Observation is the target's state during a verification window.
type Observation struct {
	DesiredReplicas int32
	ReadyReplicas   int32

	// ReadyBefore is how many replicas were ready when the window started.
	ReadyBefore     int32
	RolloutComplete bool
	// Updated is set once every pod runs the applied template, healthy or
	// not: a workload put back as it was may still be failing.
	Updated bool

	// RestartDelta and NewOOMKills count trouble since the window started.
	RestartDelta int32
	NewOOMKills  int

	// ForeignSpecChange is set when something else changed the spec.
	ForeignSpecChange bool

	// StableFor is how long the target has been healthy without a break.
	StableFor time.Duration
}

// Healthy is a complete rollout with every replica ready and no new trouble.
func (o Observation) Healthy() bool {
	return o.RolloutComplete && o.ReadyReplicas >= o.DesiredReplicas && o.RestartDelta == 0 && o.NewOOMKills == 0
}

type Criteria struct {
	Window    time.Duration
	Stability time.Duration
}

// Evaluate judges an observation elapsed into the window. The outcome is
// final once known; before the window ends only success or contamination is.
func Evaluate(o Observation, c Criteria, elapsed time.Duration) (Outcome, bool) {
	switch {
	case o.ForeignSpecChange:
		return Inconclusive, true
	case o.Healthy() && o.StableFor >= c.Stability:
		return Resolved, true
	case elapsed < c.Window:
		return "", false
	case o.ReadyReplicas < o.ReadyBefore:
		return Worsened, true
	case o.RolloutComplete && o.ReadyReplicas >= o.DesiredReplicas && o.NewOOMKills == 0:
		return Contained, true
	default:
		return Unchanged, true
	}
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// Explain says in words what verification found.
func Explain(outcome Outcome, o Observation) string {
	switch outcome {
	case Resolved:
		return fmt.Sprintf("The fix worked: %d of %d pods are ready and stayed healthy.", o.ReadyReplicas, o.DesiredReplicas)
	case Contained:
		return fmt.Sprintf("The fix helped, but the workload isn't stable: all %d pods are ready, yet containers restarted %s since.", o.ReadyReplicas, times(int(o.RestartDelta)))
	case Worsened:
		return fmt.Sprintf("The fix made things worse: %d of %d pods are ready, down from %d.", o.ReadyReplicas, o.DesiredReplicas, o.ReadyBefore)
	case Inconclusive:
		return "Can't tell whether the fix worked: the workload was changed outside Themis while Themis was checking it."
	case Unchanged:
		msg := fmt.Sprintf("The fix didn't help: %d of %d pods are ready", o.ReadyReplicas, o.DesiredReplicas)
		if o.NewOOMKills > 0 {
			msg += fmt.Sprintf(", and containers ran out of memory %s since", times(o.NewOOMKills))
		}
		return msg + "."
	default:
		return string(outcome)
	}
}
