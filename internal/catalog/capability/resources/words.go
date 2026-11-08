package resources

import (
	"encoding/json"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/catalog/capability"
)

// Words name the change by what it does to the values: a raise when every
// value it changes goes up, a decrease when every one goes down, and a change
// otherwise. One value is named with what it was and what it becomes;
// several by name only, the change itself shows them. Without the fix's
// record, only what params set is known.
func (Capability) Words(target string, params map[string]any, raw json.RawMessage) capability.Words {
	var ds []diff
	only := false
	if rec, err := readRecord(raw); err == nil {
		ds, _ = rec.diffs()
		only = rec.Only
	}
	known := len(ds) > 0
	if !known {
		a, err := parse(params)
		if err != nil {
			return capability.Say("change", capability.VerbChange, "the resources of "+target, capability.PodsStayReady)
		}
		ds = a.diffs()
	}

	var containers, names []string
	for _, d := range ds {
		if !slices.Contains(containers, d.container) {
			containers = append(containers, d.container)
		}
		if !slices.Contains(names, d.field.String()) {
			names = append(names, d.field.String())
		}
	}
	if len(containers) > 1 {
		for i := range names {
			names[i] += "s"
		}
	}
	what := capability.Of(target, containers, known && only, capability.And(names))

	noun, verb := "change", capability.VerbChange
	switch way(ds, "") {
	case "increase":
		noun, verb = "raise", capability.VerbRaise
	case "decrease":
		noun, verb = "decrease", capability.VerbLower
	}
	if len(ds) == 1 {
		if d := ds[0]; d.before == nil {
			verb, what = capability.VerbSet, what+" to "+shown(d.after)
		} else {
			what += " from " + shown(d.before) + " to " + shown(d.after)
		}
	}

	verifies := capability.PodsStayReady
	switch way(ds, corev1.ResourceMemory) {
	case "":
	case "increase":
		verifies = "the pods stop being OOMKilled and stay ready"
	default:
		verifies += ", with no OOM kill"
	}
	// Ready pods can be throttled: a CPU raise is checked by how often the
	// new pods are throttled at their limit, as the profiles that offer one
	// name in their verification checks.
	if way(ds, corev1.ResourceCPU) == "increase" {
		verifies += ", and are no longer throttled at their CPU limit"
	}
	return capability.Say(noun, verb, what, verifies)
}

// way says how ds change the values of res, or of every resource when res
// is empty: increase or decrease when all of them go that way, mixed when
// they don't or one is set where there was none, and empty for none.
func way(ds []diff, res corev1.ResourceName) string {
	out := ""
	for _, d := range ds {
		if res != "" && d.field.resource != res {
			continue
		}
		w := "mixed"
		switch {
		case d.before == nil:
		case d.after.Cmp(*d.before) > 0:
			w = "increase"
		case d.after.Cmp(*d.before) < 0:
			w = "decrease"
		}
		if out != "" && out != w {
			w = "mixed"
		}
		out = w
	}
	return out
}

// diffs are the values a sets, as changes from values not known: by
// container name, in the order fields are shown.
func (a asked) diffs() []diff {
	var out []diff
	for _, name := range a.sortedNames() {
		for _, f := range fields {
			if q, ok := a[name][f]; ok {
				out = append(out, diff{container: name, field: f, after: &q})
			}
		}
	}
	return out
}
