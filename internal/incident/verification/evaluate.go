package verification

import (
	"slices"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
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
	// Signal is how long the pods must have run healthy before the
	// profile's verification checks can be read from what they did since the
	// change: what the evidence those checks read over a window needs, such
	// as 5 minutes for CPU throttling. Zero without such checks.
	Signal time.Duration
}

// Evaluate judges a reading elapsed into the window, the target having been
// healthy for stableFor. The outcome is final once known; before the window
// ends only success or contamination is. A healthy target whose
// verification checks need longer than its stability is watched past the
// window until they can be read.
func Evaluate(r capability.Reading, stableFor time.Duration, c Criteria, elapsed time.Duration) (Outcome, bool) {
	switch {
	case r.Changed:
		return Inconclusive, true
	case r.Health == capability.Healthy && stableFor >= max(c.Stability, c.Signal):
		return Resolved, true
	case elapsed < c.Window:
		return "", false
	case r.Health == capability.Healthy && stableFor >= c.Stability:
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
		return "Fixed. The workload recovered and stayed healthy. " + r.Facts
	case Contained:
		return "Partly recovered. " + r.Facts + " Referred to a person."
	case Worsened:
		return "Worse after the fix. " + r.Facts
	case Unchanged:
		return "The fix didn't help. " + r.Facts
	case Inconclusive:
		return "Inconclusive. The workload was changed outside Themis during verification. Referred to a person."
	default:
		return string(outcome)
	}
}

// Verdict is how verification ends: its outcome, what the timeline says, and
// whether Themis puts the fix back.
type Verdict struct {
	Outcome Outcome
	Reason  string
	Restore bool
}

// Rollout is the verdict from the rollout alone: the pods' readiness,
// restarts and OOM kills. A fix that left the workload unchanged or worse is
// put back.
func Rollout(outcome Outcome, r capability.Reading) Verdict {
	return Verdict{Outcome: outcome, Reason: Explain(outcome, r), Restore: outcome == Unchanged || outcome == Worsened}
}

// kept ends a verdict that leaves the fix in place and refers the incident.
const kept = " The change stays, as it left the workload no worse. Referred to a person."

// Judge is the verdict for a fix whose profile names verification checks,
// once the rollout's outcome is final, from what those checks found since
// the change: now. before are the incident's checks from when it opened,
// to say what changed. words describe the checks.
//
// The fix is fixed only when the rollout is and every check is false. A
// check still true means it isn't, whatever the pods' readiness says; one
// unknown means Themis can't tell, and never claims a success it can't
// prove. A fix the checks judge is put back only when it left the workload
// worse: one that didn't cure the failure, but did no harm Kubernetes
// reports, such as a higher limit, stays, since putting the old values back
// only brings the failure back sooner.
func Judge(outcome Outcome, r capability.Reading, now, before []check.Result, words check.List) Verdict {
	if outcome != Resolved && outcome != Unchanged {
		return Rollout(outcome, r)
	}
	var failing, unknown, cleared []check.Result
	for _, c := range now {
		switch c.Value {
		case check.True:
			failing = append(failing, c)
		case check.Unknown:
			unknown = append(unknown, c)
		case check.False:
			cleared = append(cleared, c)
		}
	}
	switch {
	case len(failing) > 0:
		return Verdict{Outcome: Unchanged, Reason: "Not fixed. " + since(failing, words) + " " + r.Facts + kept}
	case outcome == Unchanged:
		return Verdict{Outcome: Unchanged, Reason: "The fix didn't help. " + r.Facts + kept}
	case len(unknown) > 0:
		return Verdict{Outcome: Inconclusive, Reason: "Inconclusive. " + since(unknown, words) + " " + r.Facts + " The change stays. Referred to a person."}
	}
	reason := Explain(Resolved, r) + " " + since(cleared, words)
	var was []check.Result
	for _, b := range before {
		if b.Value == check.True && slices.ContainsFunc(cleared, func(c check.Result) bool { return c.Name == b.Name }) {
			was = append(was, b)
		}
	}
	if len(was) > 0 {
		reason += " Before it, they found " + that(was, words) + "."
	}
	return Verdict{Outcome: Resolved, Reason: reason}
}

// since says what the checks found since the change: "Since the change, the
// checks found that a container is throttled at its CPU limit (…)."
func since(results []check.Result, words check.List) string {
	return "Since the change, the checks found " + that(results, words) + "."
}

// that lists what results found, each as words.Found puts it.
func that(results []check.Result, words check.List) string {
	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, "that "+words.Found(r))
	}
	return strings.Join(parts, " and ")
}
