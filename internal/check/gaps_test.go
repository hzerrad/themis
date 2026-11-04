package check_test

import (
	"slices"
	"testing"

	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/evidence"
)

// A check that couldn't answer gets the gaps in the evidence it reads. It is
// blind when Themis couldn't see that evidence, which the operator can fix,
// and ambiguous when it was there but did not settle the check.
func TestAttribute(t *testing.T) {
	unknown := rollout.Evaluate(evidence.Bundle{}, rolloutAt)
	tests := []struct {
		name    string
		results []check.Result
		gap     *check.Gap
	}{
		{"forbidden makes its readers blind", unknown, &check.Gap{Template: evidence.RolloutHistory, Cause: evidence.GapForbidden}},
		{"absent is attached but not blind", unknown, &check.Gap{Template: evidence.PodStates, Cause: evidence.GapAbsent}},
		{"no gap is ambiguous", unknown, nil},
		{"an answered check needed nothing it lacked", []check.Result{{Name: "oom_killed_new_revision", Value: check.True}}, &check.Gap{Template: evidence.PodStates, Cause: evidence.GapTimeout}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gaps []check.Gap
			if tt.gap != nil {
				gaps = []check.Gap{*tt.gap}
			}
			for _, r := range rollout.Attribute(tt.results, gaps) {
				c, _ := rollout.Get(r.Name)
				lacks := tt.gap != nil && r.Value == check.Unknown && slices.Contains(c.Reads, tt.gap.Template)
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
