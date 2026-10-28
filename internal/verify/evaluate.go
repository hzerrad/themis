package verify

import "time"

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
