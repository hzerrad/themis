package predicate_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/predicate"
)

func TestNeeds_CoversEveryCheck(t *testing.T) {
	for _, r := range predicate.Evaluate(evidence.Bundle{}, rolloutAt) {
		if len(predicate.Needs[r.Name]) == 0 {
			t.Errorf("%s declares no evidence it reads", r.Name)
		}
	}
}

// A check that couldn't answer because Themis wasn't allowed to read its
// evidence is blind: the operator can fix that.
func TestAttribute_UnknownFromForbiddenIsBlind(t *testing.T) {
	results := predicate.Attribute(predicate.Evaluate(evidence.Bundle{}, rolloutAt), []predicate.Gap{
		{Template: evidence.RolloutHistory, Cause: evidence.GapForbidden},
	})
	for _, r := range results {
		reads := false
		for _, n := range predicate.Needs[r.Name] {
			reads = reads || n == evidence.RolloutHistory
		}
		if r.Blind != reads || (len(r.Gaps) > 0) != reads {
			t.Errorf("%s: blind = %v, gaps = %v; it reads rollout history: %v", r.Name, r.Blind, r.Gaps, reads)
		}
	}
}

// A check whose evidence was there but did not settle it is not blind.
func TestAttribute_UnknownWithoutGapIsAmbiguous(t *testing.T) {
	for _, r := range predicate.Attribute(predicate.Evaluate(evidence.Bundle{}, rolloutAt), nil) {
		if r.Blind || len(r.Gaps) > 0 {
			t.Errorf("%s: blind = %v, gaps = %v; want neither", r.Name, r.Blind, r.Gaps)
		}
	}
}

// "Looked, and there was nothing" is attached, but does not make the check
// blind.
func TestAttribute_AbsentIsNotBlind(t *testing.T) {
	results := predicate.Attribute(predicate.Evaluate(evidence.Bundle{}, rolloutAt), []predicate.Gap{
		{Template: evidence.PodStates, Cause: evidence.GapAbsent},
	})
	for _, r := range results {
		if r.Name == predicate.NameOOMKilledNewRevision && (r.Blind || len(r.Gaps) != 1) {
			t.Errorf("%s: blind = %v, gaps = %v; want the absent gap, not blind", r.Name, r.Blind, r.Gaps)
		}
	}
}

// An answered check needed nothing it lacked.
func TestAttribute_AnsweredChecksGetNoGaps(t *testing.T) {
	answered := []predicate.Result{{Name: predicate.NameOOMKilledNewRevision, Value: predicate.True}}
	got := predicate.Attribute(answered, []predicate.Gap{{Template: evidence.PodStates, Cause: evidence.GapTimeout}})
	if got[0].Blind || len(got[0].Gaps) != 0 {
		t.Errorf("an answered check got gaps: %+v", got[0])
	}
}
