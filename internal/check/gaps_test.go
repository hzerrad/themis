package check_test

import (
	"slices"
	"testing"

	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/evidence"
)

func TestNeeds_CoversEveryCheck(t *testing.T) {
	for _, r := range check.Rollout().Evaluate(evidence.Bundle{}, rolloutAt) {
		if len(check.Needs[r.Name]) == 0 {
			t.Errorf("%s declares no evidence it reads", r.Name)
		}
	}
}

// A check that couldn't answer gets the gaps in the evidence it reads. It is
// blind when Themis couldn't see that evidence, which the operator can fix,
// and ambiguous when it was there but did not settle the check.
func TestAttribute(t *testing.T) {
	unknown := check.Rollout().Evaluate(evidence.Bundle{}, rolloutAt)
	tests := []struct {
		name    string
		results []check.Result
		gap     *check.Gap
	}{
		{"forbidden makes its readers blind", unknown, &check.Gap{Template: evidence.RolloutHistory, Cause: evidence.GapForbidden}},
		{"absent is attached but not blind", unknown, &check.Gap{Template: evidence.PodStates, Cause: evidence.GapAbsent}},
		{"no gap is ambiguous", unknown, nil},
		{"an answered check needed nothing it lacked", []check.Result{{Name: check.NameOOMKilledNewRevision, Value: check.True}}, &check.Gap{Template: evidence.PodStates, Cause: evidence.GapTimeout}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gaps []check.Gap
			if tt.gap != nil {
				gaps = []check.Gap{*tt.gap}
			}
			for _, r := range check.Attribute(tt.results, gaps) {
				lacks := tt.gap != nil && r.Value == check.Unknown && slices.Contains(check.Needs[r.Name], tt.gap.Template)
				want := 0
				if lacks {
					want = 1
				}
				if len(r.Gaps) != want {
					t.Errorf("%s: gaps = %v, want %d", r.Name, r.Gaps, want)
				}
				if blind := lacks && tt.gap.Cause.Blind(); r.Blind != blind {
					t.Errorf("%s: blind = %v, want %v", r.Name, r.Blind, blind)
				}
			}
		})
	}
}
