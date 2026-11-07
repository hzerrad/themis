package evidence

import (
	"cmp"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
)

// TemplateChangeView is what the current revision's pod template changed
// from the revision before it, which is what a rollback to that revision
// would undo. App is what changed in what the pods run: their containers,
// volumes and other pod settings. Metadata is what changed in the template's
// labels and annotations only, such as the annotation kubectl rollout restart
// writes: putting it back changes nothing the pods run. A value that may hold
// a secret, such as an environment variable's, a command or arguments, is
// named, never shown. Neither says what a ConfigMap or Secret holds: their
// content is no part of a revision, so a rollback doesn't put it back.
type TemplateChangeView struct {
	// From is the revision before the current one, and To the current one.
	From string `json:"from"`
	To   string `json:"to"`
	// App and Metadata each say one change in plain words, in a stable
	// order: what changed in each container, then in each init container,
	// then in the volumes, then in the other pod settings.
	App      []string `json:"app"`
	Metadata []string `json:"metadata"`
}

// maxChanges bounds each list of a TemplateChangeView.
const maxChanges = 20

// templateChange is what after changed from before.
func templateChange(before, after corev1.PodTemplateSpec) (app, metadata []string) {
	app = slices.Concat(
		containersChange("container", before.Spec.Containers, after.Spec.Containers),
		containersChange("init container", before.Spec.InitContainers, after.Spec.InitContainers),
		named("the volume", "", before.Spec.Volumes, after.Spec.Volumes, func(v corev1.Volume) string { return v.Name }),
		fieldsChange("the pod's", "", podRest(before.Spec), podRest(after.Spec)),
	)
	metadata = slices.Concat(
		mapChange("the label", before.Labels, after.Labels),
		mapChange("the annotation", before.Annotations, after.Annotations),
	)
	return bound(app), bound(metadata)
}

// bound keeps the first maxChanges changes, each cut to a message's length:
// anyone who may write the Deployment chooses its names and images.
func bound(changes []string) []string {
	out := make([]string, 0, min(len(changes), maxChanges))
	for _, c := range changes[:min(len(changes), maxChanges)] {
		out = append(out, bounded(c, maxMessageBytes))
	}
	return out
}

// containersChange is what changed in each container of after, by name,
// then which of before's are gone.
func containersChange(kind string, before, after []corev1.Container) []string {
	var out []string
	for _, a := range after {
		i := slices.IndexFunc(before, func(b corev1.Container) bool { return b.Name == a.Name })
		if i < 0 {
			out = append(out, kind+" "+a.Name+" was added")
			continue
		}
		out = append(out, containerChange(kind+" "+a.Name, before[i], a)...)
	}
	for _, b := range before {
		if !slices.ContainsFunc(after, func(a corev1.Container) bool { return a.Name == b.Name }) {
			out = append(out, kind+" "+b.Name+" was removed")
		}
	}
	return out
}

// containerChange is what changed in one container, called who.
func containerChange(who string, b, a corev1.Container) []string {
	var out []string
	if b.Image != a.Image {
		out = append(out, "the image of "+who+" changed from "+b.Image+" to "+a.Image)
	}
	if !equality.Semantic.DeepEqual(b.Command, a.Command) {
		out = append(out, "the command of "+who+" changed")
	}
	if !equality.Semantic.DeepEqual(b.Args, a.Args) {
		out = append(out, "the arguments of "+who+" changed")
	}
	out = append(out, named("the environment variable", " of "+who, b.Env, a.Env, func(e corev1.EnvVar) string { return e.Name })...)
	if !equality.Semantic.DeepEqual(b.EnvFrom, a.EnvFrom) {
		out = append(out, "the environment sources of "+who+" changed")
	}
	out = append(out, resourcesChange(who, "limit", b.Resources.Limits, a.Resources.Limits)...)
	out = append(out, resourcesChange(who, "request", b.Resources.Requests, a.Resources.Requests)...)
	for _, p := range []struct {
		kind          string
		before, after *corev1.Probe
	}{{"liveness", b.LivenessProbe, a.LivenessProbe}, {"readiness", b.ReadinessProbe, a.ReadinessProbe}, {"startup", b.StartupProbe, a.StartupProbe}} {
		switch {
		case p.before == nil && p.after != nil:
			out = append(out, "a "+p.kind+" probe was added to "+who)
		case p.before != nil && p.after == nil:
			out = append(out, "the "+p.kind+" probe of "+who+" was removed")
		case !equality.Semantic.DeepEqual(p.before, p.after):
			out = append(out, "the "+p.kind+" probe of "+who+" changed")
		}
	}
	if !equality.Semantic.DeepEqual(b.VolumeMounts, a.VolumeMounts) {
		out = append(out, "the volume mounts of "+who+" changed")
	}
	return append(out, fieldsChange("the", " of "+who, containerRest(b), containerRest(a))...)
}

// resourceNames are how people name the usual resources.
var resourceNames = map[corev1.ResourceName]string{corev1.ResourceCPU: "CPU", corev1.ResourceMemory: "memory"}

// resourcesChange is what changed in a container's limits or requests.
func resourcesChange(who, kind string, before, after corev1.ResourceList) []string {
	var out []string
	for _, name := range keys(before, after) {
		what := "the " + cmp.Or(resourceNames[name], string(name)) + " " + kind + " of " + who
		b, had := before[name]
		a, has := after[name]
		switch {
		case !had:
			out = append(out, what+" was set to "+a.String())
		case !has:
			out = append(out, what+" was removed (it was "+b.String()+")")
		case b.Cmp(a) != 0:
			out = append(out, what+" changed from "+b.String()+" to "+a.String())
		}
	}
	return out
}

// named is what changed in a list of named things: each of after that is
// new or different, then each of before that is gone, as what, the name,
// then where.
func named[T any](what, where string, before, after []T, name func(T) string) []string {
	var out []string
	for _, a := range after {
		i := slices.IndexFunc(before, func(b T) bool { return name(b) == name(a) })
		switch {
		case i < 0:
			out = append(out, what+" "+name(a)+where+" was added")
		case !equality.Semantic.DeepEqual(before[i], a):
			out = append(out, what+" "+name(a)+where+" changed")
		}
	}
	for _, b := range before {
		if !slices.ContainsFunc(after, func(a T) bool { return name(a) == name(b) }) {
			out = append(out, what+" "+name(b)+where+" was removed")
		}
	}
	return out
}

// mapChange is what changed in labels or annotations, by key.
func mapChange(what string, before, after map[string]string) []string {
	var out []string
	for _, k := range keys(before, after) {
		b, had := before[k]
		a, has := after[k]
		switch {
		case !had:
			out = append(out, what+" "+k+" was added")
		case !has:
			out = append(out, what+" "+k+" was removed")
		case a != b:
			out = append(out, what+" "+k+" changed")
		}
	}
	return out
}

// containerRest is a container's settings that containerChange doesn't name
// one by one, by their field names.
func containerRest(c corev1.Container) map[string]any {
	c.Name, c.Image, c.Command, c.Args, c.Env, c.EnvFrom = "", "", nil, nil, nil, nil
	c.Resources, c.LivenessProbe, c.ReadinessProbe, c.StartupProbe, c.VolumeMounts = corev1.ResourceRequirements{}, nil, nil, nil, nil
	return fields(&c)
}

// podRest is a pod's settings other than its containers and volumes, by
// their field names.
func podRest(s corev1.PodSpec) map[string]any {
	s.Containers, s.InitContainers, s.EphemeralContainers, s.Volumes = nil, nil, nil, nil
	return fields(&s)
}

// fields is obj by its fields' names. A typed Kubernetes object always
// converts: one that didn't would read as changed, which is the safe answer.
func fields(obj any) map[string]any {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return map[string]any{"unreadable": err.Error()}
	}
	return m
}

// fieldsChange names each field that differs between before and after, as
// prefix, the field, then where: "the pod's nodeSelector changed", "the
// ports of container app changed".
func fieldsChange(prefix, where string, before, after map[string]any) []string {
	var out []string
	for _, k := range keys(before, after) {
		if !equality.Semantic.DeepEqual(before[k], after[k]) {
			out = append(out, prefix+" "+k+where+" changed")
		}
	}
	return out
}

// keys are the keys of a and b, sorted.
func keys[K cmp.Ordered, V any](a, b map[K]V) []K {
	return slices.Sorted(maps.Keys(maps.Collect(func(yield func(K, V) bool) {
		for k, v := range a {
			if !yield(k, v) {
				return
			}
		}
		for k, v := range b {
			if !yield(k, v) {
				return
			}
		}
	})))
}
