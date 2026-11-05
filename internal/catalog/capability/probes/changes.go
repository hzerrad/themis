package probes

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/catalog/capability"
)

// edit is one probe field that differs between two templates.
type edit struct {
	container, kind, field string
	before, after          int32
}

func (e edit) lenient() bool { return lenient(e.field, e.before, e.after) }

// diff lists the probe timing fields that differ between before and after,
// for containers and probes both have, in template and display order.
func diff(before, after corev1.PodTemplateSpec) []edit {
	var out []edit
	for i := range before.Spec.Containers {
		b := &before.Spec.Containers[i]
		a := container(&after, b.Name)
		if a == nil {
			continue
		}
		for _, kind := range kinds {
			pb, pa := probe(b, kind), probe(a, kind)
			if pb == nil || pa == nil {
				continue
			}
			for _, f := range fields {
				if vb, va := value(pb, f), value(pa, f); vb != va {
					out = append(out, edit{container: b.Name, kind: kind, field: f, before: vb, after: va})
				}
			}
		}
	}
	return out
}

// direction says how edits change one probe: more_lenient, stricter, mixed,
// or unchanged.
func direction(edits []edit, container, kind string) string {
	var looser, stricter bool
	for _, e := range edits {
		if e.container == container && e.kind == kind {
			looser = looser || e.lenient()
			stricter = stricter || !e.lenient()
		}
	}
	switch {
	case looser && stricter:
		return "mixed"
	case looser:
		return "more_lenient"
	case stricter:
		return "stricter"
	}
	return "unchanged"
}

// changes lists edits for the approver: each field, before and after, saying
// which way it moves the probe, and a loosened readiness probe called out on
// a line of its own, since it sends traffic to pods that may be failing.
func changes(edits []edit) []capability.Change {
	var out []capability.Change
	called := map[string]bool{}
	for _, e := range edits {
		if e.kind == "readiness" && e.lenient() && !called[e.container] {
			called[e.container] = true
			out = append(out, capability.Change{
				Container: e.container, Field: "readiness probe",
				Before: "as it is", After: "more lenient: pods that fail it keep receiving traffic for longer",
			})
		}
		way := "stricter"
		if e.lenient() {
			way = "more lenient"
		}
		out = append(out, capability.Change{
			Container: e.container, Field: e.kind + " probe " + e.field,
			Before: strconv.Itoa(int(e.before)), After: fmt.Sprintf("%d (%s)", e.after, way),
		})
	}
	return out
}

// facts are what policy sees of edits to container, as change.<key>:
//
//	container                      the container whose probes change
//	liveness, readiness, startup   more_lenient, stricter, mixed or unchanged
//	<probe>.<field>                the field's new value, when it changes
//	<probe>.<field>.before         and its value before
func facts(edits []edit, container string) map[string]string {
	out := map[string]string{"container": container}
	for _, kind := range kinds {
		out[kind] = direction(edits, container, kind)
	}
	for _, e := range edits {
		key := e.kind + "." + e.field
		out[key] = strconv.Itoa(int(e.after))
		out[key+".before"] = strconv.Itoa(int(e.before))
	}
	return out
}

// done says what edits did, once applied.
func done(edits []edit, container string) string {
	parts := make([]string, 0, len(edits))
	for _, e := range edits {
		parts = append(parts, fmt.Sprintf("%s probe %s %d → %d", e.kind, e.field, e.before, e.after))
	}
	return fmt.Sprintf("Changed the probes of container %s: %s.", container, strings.Join(parts, ", "))
}
