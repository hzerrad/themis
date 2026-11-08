package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// Capability is deployment.rollback-to-revision as a capability.Capability.
type Capability struct{}

var (
	_ capability.Capability = Capability{}
	_ capability.Describing = Capability{}
	_ capability.Templating = Capability{}
)

func (Capability) Name() string    { return Name }
func (Capability) Version() string { return Version }
func (Capability) Noun() string    { return "a rollback" }

func (Capability) Description() string {
	return "Roll the Deployment back to the pod template of an earlier revision, as kubectl rollout undo does. revision is that revision's number, from the rollout history."
}

var schema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["revision"],` +
	`"properties":{"revision":{"type":"integer","minimum":1,"description":"The earlier revision to roll back to."}}}`)

func (Capability) Schema() json.RawMessage { return schema }

// Validate needs a revision: a positive whole number, and nothing else.
func (Capability) Validate(params map[string]any) (map[string]any, error) {
	revision, ok := RevisionParam(params)
	if !ok || len(params) != 1 {
		return nil, fmt.Errorf("%w: a rollback needs only revision, a positive whole number", capability.ErrInvalidParams)
	}
	return map[string]any{"revision": revision}, nil
}

// Plan reads the target and the template of the revision params name.
func (Capability) Plan(ctx context.Context, r client.Reader, t incident.TargetRef, params map[string]any) (capability.Plan, error) {
	revision, ok := RevisionParam(params)
	if !ok {
		return capability.Plan{}, capability.ErrInvalidParams
	}
	live, err := Read(ctx, r, t, revision)
	if errors.Is(err, ErrStale) || errors.Is(err, ErrNoSuchRevision) || errors.Is(err, ErrAlreadyCurrent) {
		return capability.Plan{}, &capability.Refusal{Reason: Explain(err, revision), NoOp: errors.Is(err, ErrAlreadyCurrent)}
	}
	if err != nil {
		return capability.Plan{}, err
	}
	rec, err := json.Marshal(newRecord(live.from, live.to, live.PreState, live.Desired))
	if err != nil {
		return capability.Plan{}, err
	}
	return capability.Plan{
		Target: live.Target, Params: Params(revision, live.DesiredHash),
		Record:      rec,
		ManagedBy:   live.ManagedBy,
		Risk:        "medium",
		BlastRadius: capability.Rollout(live.Workload),
		Done:        fmt.Sprintf("Rolled back to revision %d.", revision),
		Mark:        live.DesiredHash,
		State:       live,
	}, nil
}

// Apply writes the revision's template.
func (Capability) Apply(ctx context.Context, c client.Client, p capability.Plan, dryRun bool) (int64, error) {
	live, ok := p.State.(Live)
	if !ok {
		return 0, errors.New("not a rollback plan")
	}
	return Apply(ctx, c, live, dryRun)
}

// Undo is a rollback to the revision the rollback replaced, by the hash of
// its template.
func (Capability) Undo(ctx context.Context, r client.Reader, t incident.TargetRef, raw json.RawMessage) (string, map[string]any, error) {
	rec, _ := readRecord(raw)
	return undo(ctx, r, t, rec.From)
}

// Changes lists what the rollback changes in the pod template, as its
// record keeps them.
func (Capability) Changes(raw json.RawMessage) ([]capability.Change, error) {
	rec, err := readRecord(raw)
	if err != nil {
		return nil, err
	}
	return rec.Changes, nil
}

// Record is the record of a rollback from the template before to the one
// after, the revision params name: the revisions by their templates'
// hashes, since the templates don't say which ReplicaSets hold them.
func (Capability) Record(params map[string]any, before, after json.RawMessage) (json.RawMessage, error) {
	pre, desired, err := templates(before, after)
	if err != nil {
		return nil, err
	}
	from, err := hashWithout(pre)
	if err != nil {
		return nil, err
	}
	to, err := TemplateHash(desired)
	if err != nil {
		return nil, err
	}
	revision, _ := RevisionParam(params)
	return json.Marshal(newRecord(revisionRef{TemplateHash: from}, revisionRef{Number: revision, TemplateHash: to}, pre, desired))
}

// Words name a rollback by the revision it goes back to: "Roll checkout
// back to revision 1". Worked out against the cluster, they say how many of
// the changes to what the pods run it undoes, when it undoes several, and
// whether the image is one: nobody takes back new code without seeing it.
func (Capability) Words(target string, params map[string]any, raw json.RawMessage) capability.Words {
	to := "an earlier revision"
	if revision, ok := RevisionParam(params); ok {
		to = fmt.Sprintf("revision %d", revision)
	}
	roll := capability.Verb{Base: "Roll", Doing: "Rolling", Done: "Rolled"}
	w := capability.Say("rollback", roll, target+" back to "+to, capability.PodsStayReady)
	rec, err := readRecord(raw)
	if err != nil || rec.Undoes < 2 {
		return w
	}
	w.Note = fmt.Sprintf("Rolling back undoes %d changes", rec.Undoes)
	switch {
	case rec.Images == 1:
		w.Note += ", including the image"
	case rec.Images > 1:
		w.Note += ", including the images"
	}
	w.Note += "."
	return w
}

// Describe gives the image digests the pods run, before and after, of each
// container whose image the rollback changes.
func (Capability) Describe(ctx context.Context, _ client.Reader, p capability.Plan) (map[string]any, error) {
	live, ok := p.State.(Live)
	if !ok {
		return nil, errors.New("not a rollback plan")
	}
	return images(ctx, live.Workload, live.PreState, live.Desired)
}

// images are, for each container whose image differs between before and
// after, the digests w's pods run for each, by container: a tag can move to
// other code, a digest can't. A digest no pod runs is empty, and the page
// says it isn't known.
func images(ctx context.Context, w *workload.Workload, before, after corev1.PodTemplateSpec) (map[string]any, error) {
	out := map[string]any{}
	was := slices.Concat(before.Spec.InitContainers, before.Spec.Containers)
	for _, a := range slices.Concat(after.Spec.InitContainers, after.Spec.Containers) {
		i := slices.IndexFunc(was, func(b corev1.Container) bool { return b.Name == a.Name })
		if i < 0 || was[i].Image == a.Image {
			continue
		}
		from, err := w.Digest(ctx, a.Name, was[i].Image)
		if err != nil {
			return nil, err
		}
		to, err := w.Digest(ctx, a.Name, a.Image)
		if err != nil {
			return nil, err
		}
		out[a.Name] = map[string]any{"before": from, "after": to}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return map[string]any{"images": out}, nil
}

// RevisionParam is the revision params name: a positive whole number, as
// stored (int64) or decoded from JSON (float64).
func RevisionParam(params map[string]any) (int64, bool) {
	switch n := params["revision"].(type) {
	case int64:
		return n, n > 0
	case float64:
		return int64(n), n > 0 && n == float64(int64(n))
	}
	return 0, false
}

// Mark is the hash of the target's live pod template: a rollback leaves the
// template of the revision it restored.
func (Capability) Mark(ctx context.Context, r client.Reader, t incident.TargetRef) (string, error) {
	w, err := read(ctx, r, t)
	if err != nil {
		return "", err
	}
	return w.TemplateHash, nil
}

// Check reads the rollout of the revision the rollback restored.
func (Capability) Check(ctx context.Context, r client.Reader, t incident.TargetRef, mark string, from json.RawMessage, since time.Time) (capability.Reading, error) {
	return capability.ReadRollout(ctx, r, t, mark, from, since)
}
