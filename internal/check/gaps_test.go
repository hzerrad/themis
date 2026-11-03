package check_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/evidence"
)

func TestNeeds_CoversEveryCheck(t *testing.T) {
	for _, r := range check.Evaluate(evidence.Bundle{}, rolloutAt) {
		if len(check.Needs[r.Name]) == 0 {
			t.Errorf("%s declares no evidence it reads", r.Name)
		}
	}
}

// A check that couldn't answer because Themis wasn't allowed to read its
// evidence is blind: the operator can fix that.
func TestAttribute_UnknownFromForbiddenIsBlind(t *testing.T) {
	results := check.Attribute(check.Evaluate(evidence.Bundle{}, rolloutAt), []check.Gap{
		{Template: evidence.RolloutHistory, Cause: evidence.GapForbidden},
	})
	for _, r := range results {
		reads := false
		for _, n := range check.Needs[r.Name] {
			reads = reads || n == evidence.RolloutHistory
		}
		if r.Blind != reads || (len(r.Gaps) > 0) != reads {
			t.Errorf("%s: blind = %v, gaps = %v; it reads rollout history: %v", r.Name, r.Blind, r.Gaps, reads)
		}
	}
}

// A check whose evidence was there but did not settle it is not blind.
func TestAttribute_UnknownWithoutGapIsAmbiguous(t *testing.T) {
	for _, r := range check.Attribute(check.Evaluate(evidence.Bundle{}, rolloutAt), nil) {
		if r.Blind || len(r.Gaps) > 0 {
			t.Errorf("%s: blind = %v, gaps = %v; want neither", r.Name, r.Blind, r.Gaps)
		}
	}
}

// "Looked, and there was nothing" is attached, but does not make the check
// blind.
func TestAttribute_AbsentIsNotBlind(t *testing.T) {
	results := check.Attribute(check.Evaluate(evidence.Bundle{}, rolloutAt), []check.Gap{
		{Template: evidence.PodStates, Cause: evidence.GapAbsent},
	})
	for _, r := range results {
		if r.Name == check.NameOOMKilledNewRevision && (r.Blind || len(r.Gaps) != 1) {
			t.Errorf("%s: blind = %v, gaps = %v; want the absent gap, not blind", r.Name, r.Blind, r.Gaps)
		}
	}
}

// An answered check needed nothing it lacked.
func TestAttribute_AnsweredChecksGetNoGaps(t *testing.T) {
	answered := []check.Result{{Name: check.NameOOMKilledNewRevision, Value: check.True}}
	got := check.Attribute(answered, []check.Gap{{Template: evidence.PodStates, Cause: evidence.GapTimeout}})
	if got[0].Blind || len(got[0].Gaps) != 0 {
		t.Errorf("an answered check got gaps: %+v", got[0])
	}
}
