package resources

import (
	"encoding/json"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
)

var _ capability.Commanding = Capability{}

// Commands set the values the fix changes, one kubectl set resources per
// container, which leaves every other value as it is.
func (Capability) Commands(t incident.TargetRef, _ map[string]any, before, after json.RawMessage) []string {
	var pre, desired corev1.PodTemplateSpec
	if json.Unmarshal(before, &pre) != nil || json.Unmarshal(after, &desired) != nil {
		return nil
	}
	set := map[string]map[string][]string{}
	var containers []string
	for _, d := range diffs(pre, desired) {
		if d.after == nil {
			return nil
		}
		if set[d.container] == nil {
			set[d.container] = map[string][]string{}
			containers = append(containers, d.container)
		}
		set[d.container][d.field.kind] = append(set[d.container][d.field.kind], string(d.field.resource)+"="+d.after.String())
	}
	var out []string
	for _, c := range containers {
		args := []string{"-c", c}
		for _, kind := range []string{"limits", "requests"} {
			if values := set[c][kind]; len(values) > 0 {
				args = append(args, capability.Quote("--"+kind+"="+strings.Join(slices.Sorted(slices.Values(values)), ",")))
			}
		}
		out = append(out, capability.Kubectl(t, "set resources", args...))
	}
	return out
}
