package workload

import (
	"cmp"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
)

// Change is one way a revision's pod template differs from an earlier one's,
// such as a new image or an environment variable set otherwise: what changed,
// in plain words, and how to put it back. A value that may hold a secret,
// such as an environment variable's, a command or arguments, is named, never
// shown. Neither says what a ConfigMap or Secret holds: their content is no
// part of a revision.
type Change struct {
	// Key names the change without words: "container.app.image",
	// "container.app.env.MODE", "container.app.memory.limit",
	// "volume.config", "pod.nodeSelector", "label.version".
	Key string
	// What names what changed, to begin a sentence: "the environment
	// variable MODE of container app".
	What string
	// Says is the change as a sentence: "the environment variable MODE of
	// container app changed".
	Says string
	// Container is the container of the setting that changed, and Noun the
	// setting, such as "environment variable MODE". Container is empty for
	// what isn't one container's setting, such as a container added or
	// removed, "container sidecar", or a volume, "volume config".
	Container string
	Noun      string
	// Image is set for a change of a container's image.
	Image bool

	// undo puts the change back into a template, as the template it was
	// made from has it.
	undo func(t *corev1.PodTemplateSpec, from corev1.PodTemplateSpec)
}

// Undo puts c back into t as from, the template the change was made from,
// has it, leaving everything else in t as it is.
func (c Change) Undo(t *corev1.PodTemplateSpec, from corev1.PodTemplateSpec) {
	if c.undo != nil {
		c.undo(t, from)
	}
}

// Changes are what after changed from before. App is what changed in what
// the pods run: each container's settings, then each init container's, then
// the volumes, then the pod's other settings. Metadata is what changed in the
// template's labels and annotations only, such as the annotation kubectl
// rollout restart writes: putting it back changes nothing the pods run.
func Changes(before, after corev1.PodTemplateSpec) (app, metadata []Change) {
	app = slices.Concat(
		containersChange(containersOf, before.Spec.Containers, after.Spec.Containers),
		containersChange(initContainersOf, before.Spec.InitContainers, after.Spec.InitContainers),
		volumesChange(before.Spec.Volumes, after.Spec.Volumes),
		podChange(before.Spec, after.Spec),
	)
	metadata = slices.Concat(
		mapChange("label", "the label", labelsOf, before.Labels, after.Labels),
		mapChange("annotation", "the annotation", annotationsOf, before.Annotations, after.Annotations),
	)
	return app, metadata
}

// containerList is one of a pod's lists of containers: its kind in words and
// in a key, and where a template keeps it.
type containerList struct {
	kind, key string
	of        func(*corev1.PodSpec) *[]corev1.Container
}

var (
	containersOf     = containerList{"container", "container", func(s *corev1.PodSpec) *[]corev1.Container { return &s.Containers }}
	initContainersOf = containerList{"init container", "initContainer", func(s *corev1.PodSpec) *[]corev1.Container { return &s.InitContainers }}
)

// containersChange is what changed in each container of after, by name,
// then which of before's are gone.
func containersChange(l containerList, before, after []corev1.Container) []Change {
	var out []Change
	for _, a := range after {
		i := slices.IndexFunc(before, func(b corev1.Container) bool { return b.Name == a.Name })
		if i < 0 {
			out = append(out, whole(l, a.Name, "was added"))
			continue
		}
		out = append(out, containerChange(l, before[i], a)...)
	}
	for _, b := range before {
		if !slices.ContainsFunc(after, func(a corev1.Container) bool { return a.Name == b.Name }) {
			out = append(out, whole(l, b.Name, "was removed"))
		}
	}
	return out
}

// whole is a container added or removed: putting it back removes it, or adds
// it again where it was.
func whole(l containerList, name, how string) Change {
	what := l.kind + " " + name
	return Change{
		Key: l.key + "." + name, What: what, Says: what + " " + how, Noun: what,
		undo: func(t *corev1.PodTemplateSpec, from corev1.PodTemplateSpec) {
			byName := func(c corev1.Container) bool { return c.Name == name }
			list := l.of(&t.Spec)
			*list = slices.DeleteFunc(*list, byName)
			was := *l.of(&from.Spec)
			if i := slices.IndexFunc(was, byName); i >= 0 {
				*list = slices.Insert(*list, min(i, len(*list)), *was[i].DeepCopy())
			}
		},
	}
}

// containerChange is what changed in one container, b before and a after.
func containerChange(l containerList, b, a corev1.Container) []Change {
	who := l.kind + " " + a.Name
	key := l.key + "." + a.Name
	var out []Change
	// add is one change to the container's setting noun, and set puts it back
	// into the container as the earlier one has it.
	add := func(field, noun, says string, set func(c *corev1.Container, was corev1.Container)) {
		out = append(out, Change{
			Key: key + "." + field, What: "the " + noun + " of " + who, Says: says, Container: a.Name, Noun: noun,
			undo: inContainer(l, a.Name, set),
		})
	}
	if b.Image != a.Image {
		add("image", "image", "the image of "+who+" changed from "+b.Image+" to "+a.Image, func(c *corev1.Container, was corev1.Container) { c.Image = was.Image })
		out[len(out)-1].Image = true
	}
	if !equality.Semantic.DeepEqual(b.Command, a.Command) {
		add("command", "command", "the command of "+who+" changed", func(c *corev1.Container, was corev1.Container) { c.Command = slices.Clone(was.Command) })
	}
	if !equality.Semantic.DeepEqual(b.Args, a.Args) {
		add("args", "arguments", "the arguments of "+who+" changed", func(c *corev1.Container, was corev1.Container) { c.Args = slices.Clone(was.Args) })
	}
	for _, e := range named(b.Env, a.Env, func(e corev1.EnvVar) string { return e.Name }) {
		noun := "environment variable " + e.name
		add("env."+e.name, noun, "the "+noun+" of "+who+" "+e.how, func(c *corev1.Container, was corev1.Container) {
			c.Env = putBack(c.Env, was.Env, e.name, func(v corev1.EnvVar) string { return v.Name })
		})
	}
	if !equality.Semantic.DeepEqual(b.EnvFrom, a.EnvFrom) {
		add("envFrom", "environment sources", "the environment sources of "+who+" changed", func(c *corev1.Container, was corev1.Container) {
			c.EnvFrom = slices.Clone(was.EnvFrom)
		})
	}
	for _, kind := range []string{"limit", "request"} {
		list := func(r *corev1.ResourceRequirements) *corev1.ResourceList {
			if kind == "limit" {
				return &r.Limits
			}
			return &r.Requests
		}
		before, after := *list(&b.Resources), *list(&a.Resources)
		for _, name := range keys(before, after) {
			noun := cmp.Or(resourceNames[name], string(name)) + " " + kind
			was, had := before[name]
			now, has := after[name]
			says := ""
			switch {
			case !had:
				says = "the " + noun + " of " + who + " was set to " + now.String()
			case !has:
				says = "the " + noun + " of " + who + " was removed (it was " + was.String() + ")"
			case was.Cmp(now) != 0:
				says = "the " + noun + " of " + who + " changed from " + was.String() + " to " + now.String()
			default:
				continue
			}
			add(string(name)+"."+kind, noun, says, func(c *corev1.Container, from corev1.Container) {
				into, earlier := list(&c.Resources), *list(&from.Resources)
				if q, ok := earlier[name]; ok {
					if *into == nil {
						*into = corev1.ResourceList{}
					}
					(*into)[name] = q.DeepCopy()
				} else {
					delete(*into, name)
				}
			})
		}
	}
	for _, p := range []struct {
		kind string
		of   func(*corev1.Container) **corev1.Probe
	}{
		{"liveness", func(c *corev1.Container) **corev1.Probe { return &c.LivenessProbe }},
		{"readiness", func(c *corev1.Container) **corev1.Probe { return &c.ReadinessProbe }},
		{"startup", func(c *corev1.Container) **corev1.Probe { return &c.StartupProbe }},
	} {
		was, now := *p.of(&b), *p.of(&a)
		noun := p.kind + " probe"
		says := ""
		switch {
		case was == nil && now != nil:
			says = "a " + noun + " was added to " + who
		case was != nil && now == nil:
			says = "the " + noun + " of " + who + " was removed"
		case !equality.Semantic.DeepEqual(was, now):
			says = "the " + noun + " of " + who + " changed"
		default:
			continue
		}
		add("probe."+p.kind, noun, says, func(c *corev1.Container, from corev1.Container) { *p.of(c) = (*p.of(&from)).DeepCopy() })
	}
	if !equality.Semantic.DeepEqual(b.VolumeMounts, a.VolumeMounts) {
		add("volumeMounts", "volume mounts", "the volume mounts of "+who+" changed", func(c *corev1.Container, was corev1.Container) {
			c.VolumeMounts = slices.Clone(was.VolumeMounts)
		})
	}
	before, after := containerRest(b), containerRest(a)
	for _, field := range keys(before, after) {
		if equality.Semantic.DeepEqual(before[field], after[field]) {
			continue
		}
		add(field, field, "the "+field+" of "+who+" changed", func(c *corev1.Container, was corev1.Container) {
			putField(c, &was, field)
		})
	}
	return out
}

// inContainer is set applied to the container named name in a template, with
// the earlier template's container of that name.
func inContainer(l containerList, name string, set func(c *corev1.Container, was corev1.Container)) func(*corev1.PodTemplateSpec, corev1.PodTemplateSpec) {
	return func(t *corev1.PodTemplateSpec, from corev1.PodTemplateSpec) {
		list, was := *l.of(&t.Spec), *l.of(&from.Spec)
		i := slices.IndexFunc(list, func(c corev1.Container) bool { return c.Name == name })
		j := slices.IndexFunc(was, func(c corev1.Container) bool { return c.Name == name })
		if i >= 0 && j >= 0 {
			set(&list[i], *was[j].DeepCopy())
		}
	}
}

// resourceNames are how people name the usual resources.
var resourceNames = map[corev1.ResourceName]string{corev1.ResourceCPU: "CPU", corev1.ResourceMemory: "memory"}

// volumesChange is what changed in the pod's volumes, by name.
func volumesChange(before, after []corev1.Volume) []Change {
	name := func(v corev1.Volume) string { return v.Name }
	var out []Change
	for _, v := range named(before, after, name) {
		what := "the volume " + v.name
		out = append(out, Change{
			Key: "volume." + v.name, What: what, Says: what + " " + v.how, Noun: "volume " + v.name,
			undo: func(t *corev1.PodTemplateSpec, from corev1.PodTemplateSpec) {
				t.Spec.Volumes = putBack(t.Spec.Volumes, from.Spec.Volumes, v.name, name)
			},
		})
	}
	return out
}

// podChange is what changed in the pod's settings other than its containers
// and volumes, by their field names.
func podChange(b, a corev1.PodSpec) []Change {
	before, after := podRest(b), podRest(a)
	var out []Change
	for _, field := range keys(before, after) {
		if equality.Semantic.DeepEqual(before[field], after[field]) {
			continue
		}
		out = append(out, Change{
			Key: "pod." + field, What: "the pod's " + field, Says: "the pod's " + field + " changed", Noun: "pod's " + field,
			undo: func(t *corev1.PodTemplateSpec, from corev1.PodTemplateSpec) { putField(&t.Spec, &from.Spec, field) },
		})
	}
	return out
}

// mapChange is what changed in labels or annotations, by key.
func mapChange(key, what string, of func(*corev1.PodTemplateSpec) *map[string]string, before, after map[string]string) []Change {
	var out []Change
	for _, k := range keys(before, after) {
		b, had := before[k]
		a, has := after[k]
		how := ""
		switch {
		case !had:
			how = "was added"
		case !has:
			how = "was removed"
		case a != b:
			how = "changed"
		default:
			continue
		}
		out = append(out, Change{
			Key: key + "." + k, What: what + " " + k, Says: what + " " + k + " " + how, Noun: key + " " + k,
			undo: func(t *corev1.PodTemplateSpec, from corev1.PodTemplateSpec) {
				m := of(t)
				if v, ok := (*of(&from))[k]; ok {
					if *m == nil {
						*m = map[string]string{}
					}
					(*m)[k] = v
				} else {
					delete(*m, k)
				}
			},
		})
	}
	return out
}

func labelsOf(t *corev1.PodTemplateSpec) *map[string]string      { return &t.Labels }
func annotationsOf(t *corev1.PodTemplateSpec) *map[string]string { return &t.Annotations }

// namedChange is one named thing of a list that changed: its name, and how,
// as the end of a sentence.
type namedChange struct{ name, how string }

// named is what changed in a list of named things: each of after that is new
// or different, then each of before that is gone.
func named[T any](before, after []T, name func(T) string) []namedChange {
	var out []namedChange
	for _, a := range after {
		i := slices.IndexFunc(before, func(b T) bool { return name(b) == name(a) })
		switch {
		case i < 0:
			out = append(out, namedChange{name(a), "was added"})
		case !equality.Semantic.DeepEqual(before[i], a):
			out = append(out, namedChange{name(a), "changed"})
		}
	}
	for _, b := range before {
		if !slices.ContainsFunc(after, func(a T) bool { return name(a) == name(b) }) {
			out = append(out, namedChange{name(b), "was removed"})
		}
	}
	return out
}

// putBack is list with its item named key as was has it: replaced, removed
// when was has none, or added again where it was.
func putBack[T any](list, was []T, key string, name func(T) string) []T {
	byName := func(v T) bool { return name(v) == key }
	j := slices.IndexFunc(was, byName)
	if i := slices.IndexFunc(list, byName); i >= 0 {
		if j < 0 {
			return slices.Delete(slices.Clone(list), i, i+1)
		}
		out := slices.Clone(list)
		out[i] = was[j]
		return out
	}
	if j < 0 {
		return list
	}
	return slices.Insert(slices.Clone(list), min(j, len(list)), was[j])
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

// putField sets the field of into, by its name, to what was has, or unsets
// it when was has none. A typed Kubernetes object always converts: one that
// didn't is left as it is.
func putField[T any](into, was *T, field string) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(into)
	if err != nil {
		return
	}
	earlier, err := runtime.DefaultUnstructuredConverter.ToUnstructured(was)
	if err != nil {
		return
	}
	if v, ok := earlier[field]; ok {
		m[field] = v
	} else {
		delete(m, field)
	}
	var out T
	if runtime.DefaultUnstructuredConverter.FromUnstructured(m, &out) == nil {
		*into = out
	}
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
