package verify_test

import (
	"strings"
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

// What verification found reads as a sentence about pods, never as a code.
func TestExplain(t *testing.T) {
	o := verify.Observation{DesiredReplicas: 3, ReadyReplicas: 1, ReadyBefore: 2, RestartDelta: 4, NewOOMKills: 2}
	for outcome, want := range map[verify.Outcome]string{
		verify.Resolved:     "The fix worked: 1 of 3 pods are ready and stayed healthy.",
		verify.Worsened:     "The fix made things worse: 1 of 3 pods are ready, down from 2.",
		verify.Unchanged:    "The fix didn't help: 1 of 3 pods are ready, and containers ran out of memory 2 times since.",
		verify.Inconclusive: "Can't tell whether the fix worked: the workload was changed outside Themis while Themis was checking it.",
	} {
		if got := verify.Explain(outcome, o); got != want {
			t.Errorf("%s: %q, want %q", outcome, got, want)
		}
	}
	if got := verify.Explain(verify.Unchanged, verify.Observation{DesiredReplicas: 3, ReadyReplicas: 1, NewOOMKills: 1}); !strings.HasSuffix(got, "ran out of memory once since.") {
		t.Errorf("one kill: %q", got)
	}
	healthy := verify.Observation{DesiredReplicas: 3, ReadyReplicas: 3, RestartDelta: 2}
	if got, want := verify.Explain(verify.Contained, healthy), "The fix helped, but the workload isn't stable: all 3 pods are ready, yet containers restarted 2 times since."; got != want {
		t.Errorf("contained: %q, want %q", got, want)
	}
	if got := verify.Explain(verify.Unchanged, healthy); strings.Contains(got, "memory") {
		t.Errorf("unchanged without memory kills mentions them: %q", got)
	}
}
