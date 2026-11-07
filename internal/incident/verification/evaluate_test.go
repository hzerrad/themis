package verification_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/incident/verification"
)

// A fix whose checks read a rate is watched until its pods have run healthy
// long enough for the rate, past the window if need be.
func TestEvaluate_WaitsForTheSignal(t *testing.T) {
	c := verification.Criteria{Window: 5 * time.Minute, Stability: 2 * time.Minute, Signal: 5 * time.Minute}
	healthy := capability.Reading{Health: capability.Healthy}
	if _, final := verification.Evaluate(healthy, 3*time.Minute, c, 6*time.Minute); final {
		t.Error("final before the pods ran long enough for the checks")
	}
	if got, final := verification.Evaluate(healthy, 5*time.Minute, c, 7*time.Minute); got != verification.Resolved || !final {
		t.Errorf("after the signal's window: %q, final %v; want resolved", got, final)
	}
	if got, _ := verification.Evaluate(capability.Reading{Health: capability.Unchanged}, 0, c, 6*time.Minute); got != verification.Unchanged {
		t.Errorf("unhealthy past the window: %q, want unchanged", got)
	}
}

// A fix is fixed only when its rollout is and its verification checks find
// the failure gone since the change. One still failing, or whose rollout
// didn't help, stays when it left the workload no worse; one Themis can't
// measure is never called fixed; one that made it worse is put back.
func TestJudge(t *testing.T) {
	defs, err := check.Parse([]byte(`
- name: cpu_throttled_at_limit
  version: "1"
  description: A container is throttled at its CPU limit
  whenFalse: No container is throttled at its CPU limit
  topic: Throttling at the CPU limit
  reads: [container_cpu]
  answers: [{answer: unknown, reason: x}]
`))
	if err != nil {
		t.Fatal(err)
	}
	words, err := check.Compile(defs, []string{"container_cpu"})
	if err != nil {
		t.Fatal(err)
	}
	r := capability.Reading{Health: capability.Healthy, Facts: "1 of 1 pods are ready."}
	throttled := func(v check.Truth, reason string) []check.Result {
		return []check.Result{{Name: "cpu_throttled_at_limit", Value: v, Reason: reason}}
	}
	before := throttled(check.True, "app was throttled in 83% of its CPU periods over 1h, at its limit of 0.2 CPU")
	for name, tc := range map[string]struct {
		outcome verification.Outcome
		reading capability.Reading
		now     []check.Result
		want    verification.Verdict
	}{
		"throttling gone": {verification.Resolved, r, throttled(check.False, "app was throttled in 2% of its CPU periods over 7m, at its limit of 0.3 CPU, below 25%"), verification.Verdict{
			Outcome: verification.Resolved,
			Reason: "Fixed. The workload recovered and stayed healthy. 1 of 1 pods are ready. Since the change, the checks found that no container is throttled at its CPU limit (app was throttled in 2% of its CPU periods over 7m, at its limit of 0.3 CPU, below 25%). " +
				"Before it, they found that a container is throttled at its CPU limit (app was throttled in 83% of its CPU periods over 1h, at its limit of 0.2 CPU).",
		}},
		"still throttled at the new limit": {verification.Resolved, r, throttled(check.True, "app was throttled in 57% of its CPU periods over 7m, at its limit of 0.3 CPU"), verification.Verdict{
			Outcome: verification.Unchanged,
			Reason: "Not fixed. Since the change, the checks found that a container is throttled at its CPU limit (app was throttled in 57% of its CPU periods over 7m, at its limit of 0.3 CPU). " +
				"1 of 1 pods are ready. The change stays, as it left the workload no worse. Referred to a person.",
		}},
		"throttling not measured": {verification.Resolved, r, throttled(check.Unknown, "no throttling figures: no metrics source is set up"), verification.Verdict{
			Outcome: verification.Inconclusive,
			Reason: "Inconclusive. Since the change, the checks found that throttling at the CPU limit is unknown (no throttling figures: no metrics source is set up). " +
				"1 of 1 pods are ready. The change stays. Referred to a person.",
		}},
		"rollout no better, no worse": {verification.Unchanged, capability.Reading{Health: capability.Unchanged, Facts: "0 of 1 pods are ready."}, throttled(check.Unknown, "no container cpu"), verification.Verdict{
			Outcome: verification.Unchanged,
			Reason:  "The fix didn't help. 0 of 1 pods are ready. The change stays, as it left the workload no worse. Referred to a person.",
		}},
		"worse": {verification.Worsened, capability.Reading{Health: capability.Worse, Facts: "0 of 1 pods are ready, down from 1."}, nil, verification.Verdict{
			Outcome: verification.Worsened, Reason: "Worse after the fix. 0 of 1 pods are ready, down from 1.", Restore: true,
		}},
	} {
		if got := verification.Judge(tc.outcome, tc.reading, tc.now, before, words); got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, tc.want)
		}
	}
}

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
		"changed by someone else":        {capability.Reading{Changed: true, Health: capability.Healthy}, time.Hour, early, verification.Inconclusive, true, "Inconclusive. The workload was changed outside Themis during verification. Referred to a person."},
		"healthy long enough":            {reading(capability.Healthy), 2 * time.Minute, early, verification.Resolved, true, "Fixed. The workload recovered and stayed healthy. "},
		"healthy, not yet long enough":   {reading(capability.Healthy), time.Minute, early, "", false, ""},
		"not healthy before the window":  {reading(capability.Unchanged), 0, early, "", false, ""},
		"healthy too briefly at the end": {reading(capability.Healthy), time.Minute, late, verification.Contained, true, "Partly recovered. "},
		"improved at the end":            {reading(capability.Improved), 0, late, verification.Contained, true, "Partly recovered. "},
		"worse at the end":               {reading(capability.Worse), 0, late, verification.Worsened, true, "Worse after the fix. "},
		"unchanged at the end":           {reading(capability.Unchanged), 0, late, verification.Unchanged, true, "The fix didn't help. "},
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
