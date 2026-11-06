package verification

import (
	"time"

	"github.com/hzerrad/themis/internal/catalog/capability"
)

type Outcome string

const (
	Resolved     Outcome = "resolved"
	Contained    Outcome = "contained"
	Unchanged    Outcome = "unchanged"
	Worsened     Outcome = "worsened"
	Inconclusive Outcome = "inconclusive"
)

type Criteria struct {
	Window    time.Duration
	Stability time.Duration
}

// Evaluate judges a reading elapsed into the window, the target having been
// healthy for stableFor. The outcome is final once known; before the window
// ends only success or contamination is.
func Evaluate(r capability.Reading, stableFor time.Duration, c Criteria, elapsed time.Duration) (Outcome, bool) {
	switch {
	case r.Changed:
		return Inconclusive, true
	case r.Health == capability.Healthy && stableFor >= c.Stability:
		return Resolved, true
	case elapsed < c.Window:
		return "", false
	case r.Health == capability.Worse:
		return Worsened, true
	case r.Health == capability.Improved, r.Health == capability.Healthy:
		return Contained, true
	default:
		return Unchanged, true
	}
}

// Explain says in words what verification found: the outcome, then what the
// fix saw, then what follows for an outcome that refers the incident to a
// person.
func Explain(outcome Outcome, r capability.Reading) string {
	switch outcome {
	case Resolved:
		return "Fixed. The workload recovered and held: " + r.Facts
	case Contained:
		return "Partly recovered: " + r.Facts + " Referred to a person."
	case Worsened:
		return "Worse after the fix: " + r.Facts
	case Unchanged:
		return "The fix didn't help: " + r.Facts
	case Inconclusive:
		return "Inconclusive: the workload was changed outside Themis while Themis verified the fix. Referred to a person."
	default:
		return string(outcome)
	}
}
