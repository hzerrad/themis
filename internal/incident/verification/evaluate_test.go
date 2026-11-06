package verification_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident/verification"
)

// What a fix's reading means over time: the fix judges its symptom, and
// verification turns that into an outcome once it is known.
func TestEvaluate(t *testing.T) {
	c := verification.Criteria{Window: 10 * time.Minute, Stability: 2 * time.Minute}
	early, late := time.Minute, 10*time.Minute
	reading := func(h capability.Health) capability.Reading {
		return capability.Reading{Health: h, Facts: "2 of 3 pods are ready."}
	}
	for name, tc := range map[string]struct {
		reading   capability.Reading
		stableFor time.Duration
		elapsed   time.Duration
		want      verification.Outcome
		final     bool
		says      string
	}{
		"changed by someone else":        {capability.Reading{Changed: true, Health: capability.Healthy}, time.Hour, early, verification.Inconclusive, true, "Inconclusive: the workload was changed outside Themis while Themis verified the fix. Referred to a person."},
		"healthy long enough":            {reading(capability.Healthy), 2 * time.Minute, early, verification.Resolved, true, "Fixed. The workload recovered and held: "},
		"healthy, not yet long enough":   {reading(capability.Healthy), time.Minute, early, "", false, ""},
		"not healthy before the window":  {reading(capability.Unchanged), 0, early, "", false, ""},
		"healthy too briefly at the end": {reading(capability.Healthy), time.Minute, late, verification.Contained, true, "Partly recovered: "},
		"improved at the end":            {reading(capability.Improved), 0, late, verification.Contained, true, "Partly recovered: "},
		"worse at the end":               {reading(capability.Worse), 0, late, verification.Worsened, true, "Worse after the fix: "},
		"unchanged at the end":           {reading(capability.Unchanged), 0, late, verification.Unchanged, true, "The fix didn't help: "},
	} {
		got, final := verification.Evaluate(tc.reading, tc.stableFor, c, tc.elapsed)
		if got != tc.want || final != tc.final {
			t.Errorf("%s: %q, final %v; want %q, final %v", name, got, final, tc.want, tc.final)
			continue
		}
		if !final {
			continue
		}
		says := verification.Explain(got, tc.reading)
		if !strings.HasPrefix(says, tc.says) || !strings.Contains(says, tc.reading.Facts) {
			t.Errorf("%s: explained as %q, want %q and what the fix saw", name, says, tc.says)
		}
	}
}
