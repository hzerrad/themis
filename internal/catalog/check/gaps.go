package check

import (
	"slices"

	"github.com/hzerrad/themis/internal/catalog/evidence"
)

// Gap is evidence a check needed and could not have, and why.
type Gap struct {
	Template string
	Cause    evidence.GapCause
}

// Attribute ties each unknown result to the gaps in the evidence its check
// reads. A result is blind when one of those gaps is: Themis could not see.
// An unknown result with no blind gap is ambiguous: the evidence did not
// settle it.
func (l List) Attribute(results []Result, gaps []Gap) []Result {
	out := slices.Clone(results)
	for i, r := range out {
		c, ok := l.Get(r.Name)
		if r.Value != Unknown || !ok {
			continue
		}
		for _, g := range gaps {
			if slices.Contains(c.Reads, g.Template) {
				out[i].Gaps = append(out[i].Gaps, g)
				out[i].Blind = out[i].Blind || g.Cause.Blind()
			}
		}
	}
	return out
}
