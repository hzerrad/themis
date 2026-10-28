package verify_test

import (
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/verify"
)

var criteria = verify.Criteria{Window: 5 * time.Minute, Stability: 2 * time.Minute}

func healthy() verify.Observation {
	return verify.Observation{DesiredReplicas: 3, ReadyReplicas: 3, ReadyBefore: 3, RolloutComplete: true, StableFor: 3 * time.Minute}
}

func evaluate(t *testing.T, o verify.Observation, elapsed time.Duration, want verify.Outcome, final bool) {
	t.Helper()
	got, done := verify.Evaluate(o, criteria, elapsed)
	if done != final || (final && got != want) {
		t.Errorf("Evaluate = %s, final %v; want %s, final %v", got, done, want, final)
	}
}

func TestEvaluate_StableReady_Resolved(t *testing.T) {
	evaluate(t, healthy(), time.Minute*3, verify.Resolved, true)
}

func TestEvaluate_NotFinalBeforeWindowEnd(t *testing.T) {
	o := healthy()
	o.StableFor = 30 * time.Second
	evaluate(t, o, time.Minute, "", false)
}

func TestEvaluate_ForeignChange_Inconclusive(t *testing.T) {
	o := healthy()
	o.ForeignSpecChange = true
	evaluate(t, o, time.Second, verify.Inconclusive, true)
}

func TestEvaluate_StillOOM_AtWindowEnd_Unchanged(t *testing.T) {
	o := healthy()
	o.ReadyReplicas, o.NewOOMKills, o.RolloutComplete, o.StableFor = 3, 2, false, 0
	evaluate(t, o, criteria.Window, verify.Unchanged, true)
}

func TestEvaluate_FewerReadyThanBefore_Worsened(t *testing.T) {
	o := healthy()
	o.ReadyReplicas, o.RolloutComplete, o.StableFor = 1, false, 0
	evaluate(t, o, criteria.Window, verify.Worsened, true)
}

// Ready but restarting: better, not fixed.
func TestEvaluate_ReadyButUnstable_Contained(t *testing.T) {
	o := healthy()
	o.RestartDelta, o.StableFor = 2, 0
	evaluate(t, o, criteria.Window, verify.Contained, true)
}
