package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// Capability is deployment.set-resources as a capability.Capability.
type Capability struct{}

var (
	_ capability.Capability  = Capability{}
	_ capability.Feasibility = Capability{}
	_ capability.Templating  = Capability{}
)

func (Capability) Name() string    { return Name }
func (Capability) Version() string { return Version }
func (Capability) Noun() string    { return "a memory or CPU change" }

func (Capability) Description() string {
	return "Set the memory and CPU requests and limits of containers in the Deployment's pod template, which rolls out new pods. " +
		"containers maps each container's name to the requests and limits to set, as Kubernetes quantities such as 512Mi or 250m; " +
		"values not named stay as they are. Choose the values from the evidence: Themis checks that they are valid and that the cluster " +
		"can run them, never how big they are. A limit can't be removed, and a request can't end above its limit."
}

var schema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["containers"],"properties":{` +
	`"containers":{"type":"object","minProperties":1,"description":"The values to set, by container name.",` +
	`"additionalProperties":{"type":"object","additionalProperties":false,"minProperties":1,"properties":{` +
	`"requests":{"type":"object","additionalProperties":false,"minProperties":1,"properties":{` +
	`"memory":{"type":"string","description":"A memory quantity, such as 512Mi."},` +
	`"cpu":{"type":"string","description":"A CPU quantity, such as 250m."}}},` +
	`"limits":{"type":"object","additionalProperties":false,"minProperties":1,"properties":{` +
	`"memory":{"type":"string","description":"A memory quantity, such as 1Gi."},` +
	`"cpu":{"type":"string","description":"A CPU quantity, such as 500m."}}}}}}}}`)

func (Capability) Schema() json.RawMessage { return schema }

// Validate needs containers, each with memory or CPU requests or limits,
// as positive Kubernetes quantities, and nothing else.
func (Capability) Validate(params map[string]any) (map[string]any, error) {
	a, err := parse(params)
	if err != nil {
		return nil, err
	}
	return a.params(), nil
}

// planned is the fix worked out: the workload as read, and the template it
// writes.
type planned struct {
	workload *workload.Workload
	after    corev1.PodTemplateSpec
	changed  []diff
}

// Plan reads the target and sets the values params ask for in its template.
func (Capability) Plan(ctx context.Context, r client.Reader, t incident.TargetRef, params map[string]any) (capability.Plan, error) {
	a, err := parse(params)
	if err != nil {
		return capability.Plan{}, err
	}
	w, err := workload.Read(ctx, r, t)
	if e, ok := errors.AsType[*workload.Error](err); ok {
		return capability.Plan{}, &capability.Refusal{Reason: fmt.Sprintf("The workload %s, so the fix no longer applies.", e.Case)}
	}
	if err != nil {
		return capability.Plan{}, err
	}
	after := *w.Template.DeepCopy()
	for _, name := range a.sortedNames() {
		i := slices.IndexFunc(after.Spec.Containers, func(c corev1.Container) bool { return c.Name == name })
		if i < 0 {
			return capability.Plan{}, &capability.Refusal{Reason: fmt.Sprintf("The workload has no container named %s. Its containers are %s.", name, names(after))}
		}
		c := &after.Spec.Containers[i]
		for _, f := range fields {
			if q, ok := a[name][f]; ok {
				f.set(c, q)
			}
		}
		for _, res := range []corev1.ResourceName{corev1.ResourceMemory, corev1.ResourceCPU} {
			request, limit := field{"requests", res}, field{"limits", res}
			if req, lim := request.get(c), limit.get(c); req != nil && lim != nil && req.Cmp(*lim) > 0 {
				return capability.Plan{}, &capability.Refusal{Reason: fmt.Sprintf(
					"%s would have a %s of %s, above its %s of %s; Kubernetes doesn't allow a request above its limit.", name, request, req, limit, lim)}
			}
		}
	}
	changed := diffs(w.Template, after)
	if len(changed) == 0 {
		return capability.Plan{}, &capability.Refusal{Reason: "Nothing to change. The workload already has these resources.", NoOp: true}
	}
	mark, err := workload.TemplateHash(after)
	if err != nil {
		return capability.Plan{}, err
	}
	rec, err := json.Marshal(newRecord(changed, len(after.Spec.Containers), w.TemplateHash, mark))
	if err != nil {
		return capability.Plan{}, err
	}
	return capability.Plan{
		Target: w.Identity(), Params: a.params(),
		Record:      rec,
		ManagedBy:   w.ManagedBy,
		Risk:        "medium",
		BlastRadius: capability.Rollout(w),
		Facts:       facts(changed),
		Done:        Capability{}.Words(t.Name, a.params(), rec).Done,
		Mark:        mark,
		State:       planned{workload: w, after: after, changed: changed},
	}, nil
}

// Apply writes the planned template.
func (Capability) Apply(ctx context.Context, c client.Client, p capability.Plan, dryRun bool) (int64, error) {
	s, ok := p.State.(planned)
	if !ok {
		return 0, errors.New("not a set-resources plan")
	}
	return s.workload.SetTemplate(ctx, c, s.after, dryRun)
}

// Undo sets back every value the fix changed that differs now from what it
// was, as the fix's record keeps it. A value the template didn't have can't
// be put back: the fix never removes one.
func (Capability) Undo(ctx context.Context, r client.Reader, t incident.TargetRef, raw json.RawMessage) (string, map[string]any, error) {
	rec, err := readRecord(raw)
	var ds []diff
	if err == nil {
		ds, err = rec.diffs()
	}
	if err != nil {
		return "", nil, &capability.Refusal{Reason: "Not restored. Themis kept no record of the resources from before it acted. The fix stays in place."}
	}
	w, err := workload.Read(ctx, r, t)
	if e, ok := errors.AsType[*workload.Error](err); ok {
		return "", nil, &capability.Refusal{Reason: fmt.Sprintf("Not restored. The workload %s.", e.Case)}
	}
	if err != nil {
		return "", nil, err
	}
	back := asked{}
	for _, d := range ds {
		i := slices.IndexFunc(w.Template.Spec.Containers, func(c corev1.Container) bool { return c.Name == d.container })
		if i < 0 || equal(d.field.get(&w.Template.Spec.Containers[i]), d.before) {
			continue
		}
		if d.before == nil {
			return "", nil, &capability.Refusal{Reason: fmt.Sprintf(
				"Not restored. Container %s had no %s before, and Themis never removes one. The fix stays in place.", d.container, d.field)}
		}
		if back[d.container] == nil {
			back[d.container] = map[field]resource.Quantity{}
		}
		back[d.container][d.field] = *d.before
	}
	if len(back) == 0 {
		return "", nil, &capability.Refusal{Reason: "Nothing to restore. The workload's resources are already as they were before Themis acted.", NoOp: true}
	}
	return Name, back.params(), nil
}

// Changes lists, for each container, the values the fix changes, as its
// record keeps them.
func (Capability) Changes(raw json.RawMessage) ([]capability.Change, error) {
	rec, err := readRecord(raw)
	if err != nil {
		return nil, err
	}
	ds, err := rec.diffs()
	if err != nil {
		return nil, err
	}
	var out []capability.Change
	for _, d := range ds {
		out = append(out, d.change())
	}
	return out, nil
}

// Record is the record of a change from the template before to the one
// after: the values it sets, and the hashes of both.
func (Capability) Record(_ map[string]any, before, after json.RawMessage) (json.RawMessage, error) {
	var pre, desired corev1.PodTemplateSpec
	if err := json.Unmarshal(before, &pre); err != nil {
		return nil, fmt.Errorf("template before the fix: %w", err)
	}
	if err := json.Unmarshal(after, &desired); err != nil {
		return nil, fmt.Errorf("template the fix writes: %w", err)
	}
	from, err := workload.TemplateHash(pre)
	if err != nil {
		return nil, err
	}
	to, err := workload.TemplateHash(desired)
	if err != nil {
		return nil, err
	}
	return json.Marshal(newRecord(diffs(pre, desired), len(pre.Spec.Containers), from, to))
}

// Mark is the hash of the target's live pod template.
func (Capability) Mark(ctx context.Context, r client.Reader, t incident.TargetRef) (string, error) {
	w, err := workload.Read(ctx, r, t)
	if err != nil {
		return "", err
	}
	return w.TemplateHash, nil
}

// Check reads the rollout of the new resources: the pods run them, stay
// ready, don't restart and don't run out of memory.
func (Capability) Check(ctx context.Context, r client.Reader, t incident.TargetRef, mark string, from json.RawMessage, since time.Time) (capability.Reading, error) {
	return capability.ReadRollout(ctx, r, t, mark, from, since)
}
