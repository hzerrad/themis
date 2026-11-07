package check_test

import (
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident"
)

// A check writes its answer as a fact and a headline for people, with the
// values of that answer, and what they read never changes the answer: a
// fact that reads evidence the answer doesn't need makes it need nothing
// more, and a fact that fails to evaluate is left out.
func TestFacts(t *testing.T) {
	const defs = `
- name: oom
  version: "1"
  description: A container was OOMKilled
  reads: [pod_states, container_memory]
  let:
    killed: pod_states.pods.filter(p, p.containers.exists(c, c.lastTermination != null && c.lastTermination.reason == "OOMKilled"))
  answers:
    - when: size(killed) > 0
      answer: true
      reason: ${killed[0].name} was OOMKilled
      cite: [ids.pod_states]
      fact: >-
        Pod ` + "`${killed[0].name}`" + ` was OOMKilled at ${killed[0].containers[0].lastTermination.finishedAt},
        peaking at ${string(container_memory.containers[0].peakBytes / 1048576)}Mi.
      headline: ${workload} runs out of memory.
    - answer: false
      reason: no OOM kill
      fact: No container was OOMKilled in ${killed[5].name}.
`
	parsed, err := check.Parse([]byte(defs))
	if err != nil {
		t.Fatal(err)
	}
	list, err := check.Compile(parsed, []string{evidence.PodStates, evidence.ContainerMemory})
	if err != nil {
		t.Fatal(err)
	}
	target := incident.TargetRef{Name: "checkout"}
	peak := int64(57 << 20)
	memory := item(t, "m", evidence.ContainerMemory, evidence.ContainerMemoryView{Containers: []evidence.MemoryUseView{{Name: "app", PeakBytes: &peak}}})

	r := list.Evaluate(evidence.Bundle{Target: target, Items: []evidence.Item{item(t, "p", evidence.PodStates, pods(oomPod())), memory}}, now)[0]
	if r.Value != check.True || r.Reason != "checkout-7d9f8b6c4-x2kqp was OOMKilled" {
		t.Fatalf("answer = %s %q", r.Value, r.Reason)
	}
	if want := "Pod `checkout-7d9f8b6c4-x2kqp` was OOMKilled at 2026-10-27T09:14:00Z, peaking at 57Mi."; r.Fact != want {
		t.Errorf("fact = %q, want %q", r.Fact, want)
	}
	if r.Headline != "checkout runs out of memory." {
		t.Errorf("headline = %q", r.Headline)
	}

	// Without container_memory, which only the fact reads, the answer stands
	// and the fact is left out.
	r = list.Evaluate(evidence.Bundle{Target: target, Items: []evidence.Item{item(t, "p", evidence.PodStates, pods(oomPod()))}}, now)[0]
	if r.Value != check.True || r.Fact != "" || r.Headline == "" {
		t.Errorf("without the memory figures = %s, fact %q, headline %q", r.Value, r.Fact, r.Headline)
	}

	// A fact whose expression fails is left out; the answer is the same.
	r = list.Evaluate(evidence.Bundle{Target: target, Items: []evidence.Item{item(t, "p", evidence.PodStates, pods(healthyPod("a", "2"))), memory}}, now)[0]
	if r.Value != check.False || r.Reason != "no OOM kill" || r.Fact != "" {
		t.Errorf("a failing fact = %s %q, fact %q", r.Value, r.Reason, r.Fact)
	}
}

// A fact may read only the evidence its check reads.
func TestFactsReadWhatTheCheckReads(t *testing.T) {
	defs, err := check.Parse([]byte(`
- name: has_pods
  version: "1"
  description: The workload runs pods
  reads: [pod_states]
  answers:
    - answer: true
      reason: pods
      fact: Uses ${container_memory.window}.
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = check.Compile(defs, []string{evidence.PodStates, evidence.ContainerMemory})
	if want := "check has_pods: answer 1: fact reads evidence template container_memory, which reads doesn't list"; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %v, want %q", err, want)
	}
}
