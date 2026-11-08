package resources

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
)

var _ capability.Commanding = Capability{}

// Commands set the values the fix changes, as its record keeps them, one
// kubectl set resources per container, which leaves every other value as it
// is.
func (Capability) Commands(t incident.TargetRef, _ map[string]any, raw json.RawMessage) []string {
	rec, err := readRecord(raw)
	if err != nil {
		return nil
	}
	ds, err := rec.diffs()
	if err != nil {
		return nil
	}
	set := map[string]map[string][]string{}
	var containers []string
	for _, d := range ds {
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
