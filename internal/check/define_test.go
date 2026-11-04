package check_test

import (
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/evidence"
)

// A definition is compiled when Themis starts, and one that doesn't compile
// is refused with a message naming the check.
func TestCompile(t *testing.T) {
	const valid = `
- name: has_pods
  version: "1"
  description: The workload runs pods
  reads: [pod_states]
  answers:
    - when: size(pod_states.pods) > 0
      answer: true
      reason: ${size(pod_states.pods)} pods
      cite: [ids.pod_states]
    - answer: false
      reason: no pods
`
	tests := []struct {
		name, edit, with, want string
	}{
		{"valid", "", "", ""},
		{"bad CEL", "size(pod_states.pods) > 0", "size(pod_states.pods) >", "check has_pods: answer 1: when: ERROR"},
		{"unknown evidence template", "reads: [pod_states]", "reads: [pod_state]", `check has_pods: reads unknown evidence template "pod_state"`},
		{"missing reason", "reason: no pods", "cite: [ids.pod_states]", "check has_pods: answer 2: reason is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defs, err := check.Parse([]byte(strings.Replace(valid, tt.edit, tt.with, 1)))
			if err != nil {
				t.Fatal(err)
			}
			_, err = check.Compile(defs, []string{evidence.PodStates})
			if got := errText(err); tt.want == "" && got != "" || !strings.HasPrefix(got, tt.want) {
				t.Errorf("error = %q, want it to start %q", got, tt.want)
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
