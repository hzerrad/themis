package probes

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/catalog/capability"
)

// What each probe field is called in a sentence.
var fieldWords = map[string]string{
	"initialDelaySeconds": "initial delay", "periodSeconds": "period", "timeoutSeconds": "timeout",
	"failureThreshold": "failure threshold", "successThreshold": "success threshold",
}

// The verbs for making a probe's time longer and shorter.
var (
	lengthen = capability.Verb{Base: "Lengthen", Doing: "Lengthening", Done: "Lengthened"}
	shorten  = capability.Verb{Base: "Shorten", Doing: "Shortening", Done: "Shortened"}
)

// Words name a change to one field by what it does to it, "Lengthen
// inventory's liveness probe timeout from 1s to 3s", and a change to several
// by the probes they belong to, "Change inventory's liveness probe". Without
// before and after, only what params set is known.
func (Capability) Words(target string, params map[string]any, before, after json.RawMessage) capability.Words {
	var pre, desired corev1.PodTemplateSpec
	var edits []edit
	if json.Unmarshal(before, &pre) == nil && json.Unmarshal(after, &desired) == nil {
		edits = diff(pre, desired)
	}
	known := len(edits) > 0
	if !known {
		r, err := parse(params, false)
		if err != nil {
			return capability.Say("probe change", capability.VerbChange, "the probes of "+target, capability.PodsStayReady)
		}
		edits = r.edits()
	}

	var probes []string
	for _, e := range edits {
		if !slices.Contains(probes, e.kind) {
			probes = append(probes, e.kind)
		}
	}
	verifies := capability.PodsStayReady
	if slices.Contains(probes, "liveness") || slices.Contains(probes, "startup") {
		verifies = capability.PodsNoRestart
	}
	of := func(what string) string {
		return capability.Of(target, []string{edits[0].container}, known && len(pre.Spec.Containers) == 1, what)
	}
	if len(edits) > 1 {
		what := capability.And(probes) + " probe"
		if len(probes) > 1 {
			what += "s"
		}
		return capability.Say("probe change", capability.VerbChange, of(what), verifies)
	}

	e := edits[0]
	what := of(e.kind + " probe " + fieldWords[e.field])
	if !known {
		return capability.Say("probe change", capability.VerbSet, what+" to "+amount(e.field, e.after), verifies)
	}
	verb := capability.VerbRaise
	switch longer := e.after > e.before; {
	case strings.HasSuffix(e.field, "Seconds") && longer:
		verb = lengthen
	case strings.HasSuffix(e.field, "Seconds"):
		verb = shorten
	case !longer:
		verb = capability.VerbLower
	}
	return capability.Say("probe change", verb, what+" from "+amount(e.field, e.before)+" to "+amount(e.field, e.after), verifies)
}

// amount is a field's value as people read it: 3s for a time, 3 for a count.
func amount(field string, v int32) string {
	n := strconv.Itoa(int(v))
	if strings.HasSuffix(field, "Seconds") {
		return n + "s"
	}
	return n
}

// edits are the fields r sets, as changes from values not known, in the
// order probes and fields are shown.
func (r request) edits() []edit {
	var out []edit
	for _, kind := range kinds {
		for _, f := range fields {
			if v, ok := r.set[kind][f]; ok {
				out = append(out, edit{container: r.container, kind: kind, field: f, after: v})
			}
		}
	}
	return out
}
