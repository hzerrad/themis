package predicate

import (
	"slices"

	"github.com/hzerrad/themis/internal/evidence"
)

// Needs names the evidence each check reads.
var Needs = map[string][]string{
	NameRolloutRecent:              {evidence.RolloutHistory},
	NameFailureOnsetAfterRollout:   {evidence.RolloutHistory, evidence.PodStates},
	NamePriorRevisionWasHealthy:    {evidence.RolloutHistory, evidence.PodStates},
	NameOOMKilledNewRevision:       {evidence.PodStates},
	NameGitOpsManaged:              {evidence.RolloutHistory},
	NameConfigChangedBeforeFailure: {evidence.ReferencedConfig, evidence.RolloutHistory, evidence.PodStates},
}

// Gap is evidence a check needed and could not have, and why.
type Gap struct {
	Template string
	Cause    evidence.GapCause
}

// Attribute ties each unknown result to the gaps in the evidence it reads. A
// result is blind when one of those gaps is: Themis could not see. An
// unknown result with no blind gap is ambiguous: the evidence did not settle
// it.
func Attribute(results []Result, gaps []Gap) []Result {
	out := slices.Clone(results)
	for i, r := range out {
		if r.Value != Unknown {
			continue
		}
		for _, g := range gaps {
			if slices.Contains(Needs[r.Name], g.Template) {
				out[i].Gaps = append(out[i].Gaps, g)
				out[i].Blind = out[i].Blind || g.Cause.Blind()
			}
		}
	}
	return out
}
