package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// PartialName is the partial rollback: it puts some of the latest rollout's
// changes back as the revision before it had them, and keeps the others,
// such as a new image.
const (
	PartialName    = "deployment.roll-back-changes"
	PartialVersion = "1"
)

// Partial is deployment.roll-back-changes as a capability.Capability. The
// changes it may roll back are those the latest rollout made to what the
// pods run, read from the two revisions' ReplicaSets, never from what a
// suggestion says: each is one of the rollout's changes, or the fix is
// refused.
type Partial struct{}

var (
	_ capability.Capability = Partial{}
	_ capability.Describing = Partial{}
	_ capability.Templating = Partial{}
)

func (Partial) Name() string    { return PartialName }
func (Partial) Version() string { return PartialVersion }
func (Partial) Noun() string    { return "a partial rollback" }

func (Partial) Description() string {
	return "Roll back some of the changes the latest rollout made to the Deployment's pod template, and keep the others, " +
		"such as putting an environment variable back while the new image stays. revision is the revision before the current one, " +
		"from the rollout history; changes are the changes to roll back, each written exactly as rollout_history's change.app lists it."
}

// maxSelected bounds the changes a suggestion may name.
const maxSelected = 50

var partialSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["revision","changes"],` +
	`"properties":{"revision":{"type":"integer","minimum":1,"description":"The revision before the current one, whose values the changes go back to."},` +
	`"changes":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"string","minLength":1,"maxLength":300},` +
	`"description":"The changes to roll back, each exactly as rollout_history's change.app lists it."}}}`)

func (Partial) Schema() json.RawMessage { return partialSchema }

// Validate needs a revision, a positive whole number, and the changes to roll
// back: one to 50 sentences, each named once.
func (Partial) Validate(params map[string]any) (map[string]any, error) {
	revision, ok := RevisionParam(params)
	changes, named := changesParam(params)
	if !ok || !named || len(params) != 2 {
		return nil, fmt.Errorf("%w: a partial rollback needs revision, a positive whole number, and changes, the rollout's changes to roll back", capability.ErrInvalidParams)
	}
	return map[string]any{"revision": revision, "changes": changes}, nil
}

// changesParam is the changes params name, each once, in the order given.
func changesParam(params map[string]any) ([]any, bool) {
	var list []string
	switch v := params["changes"].(type) {
	case []string:
		list = v
	case []any:
		for _, s := range v {
			str, ok := s.(string)
			if !ok {
				return nil, false
			}
			list = append(list, str)
		}
	default:
		return nil, false
	}
	var out []any
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" || len(s) > 1024 {
			return nil, false
		}
		if !slices.Contains(out, any(s)) {
			out = append(out, s)
		}
	}
	return out, len(out) > 0 && len(out) <= maxSelected
}

// partialBefore is how an earlier version of Themis stored the state a
// partial rollback replaced: the template it replaces, and the earlier
// revision's, which the changes go back to.
type partialBefore struct {
	Template corev1.PodTemplateSpec `json:"template"`
	Earlier  corev1.PodTemplateSpec `json:"earlier"`
}

// partialLive is the target as a partial rollback found it, for Apply.
type partialLive struct {
	Workload *workload.Workload
	Desired  corev1.PodTemplateSpec
}

// Plan reads the target, the revision before the current one, and what the
// latest rollout changed between them; it refuses any change named that
// isn't one of them.
func (Partial) Plan(ctx context.Context, r client.Reader, t incident.TargetRef, params map[string]any) (capability.Plan, error) {
	revision, ok := RevisionParam(params)
	named, listed := changesParam(params)
	if !ok || !listed {
		return capability.Plan{}, capability.ErrInvalidParams
	}
	w, err := read(ctx, r, t)
	if err != nil {
		return capability.Plan{}, refused(err, revision)
	}
	earlier, err := latestRollout(ctx, w, revision)
	if err != nil {
		return capability.Plan{}, err
	}
	units, _ := workload.Changes(earlier.Template, w.Template)
	if len(units) == 0 {
		return capability.Plan{}, &capability.Refusal{Reason: fmt.Sprintf("Revision %d changed nothing the pods run, only their labels or annotations, so there is no change to roll back.", w.Revision)}
	}
	chosen, err := choose(units, named, w.Revision)
	if err != nil {
		return capability.Plan{}, err
	}
	desired := *w.Template.DeepCopy()
	for _, c := range chosen {
		c.Undo(&desired, earlier.Template)
	}
	if err := mounted(desired, units); err != nil {
		return capability.Plan{}, err
	}
	desiredHash, err := TemplateHash(desired)
	if err != nil {
		return capability.Plan{}, err
	}
	earlierHash, err := TemplateHash(earlier.Template)
	if err != nil {
		return capability.Plan{}, err
	}
	from, err := current(ctx, w)
	if err != nil {
		return capability.Plan{}, err
	}
	picked := func(u workload.Change) bool {
		return slices.ContainsFunc(chosen, func(c workload.Change) bool { return c.Key == u.Key })
	}
	rec, err := json.Marshal(newPartialRecord(from, refOf(earlier, earlierHash), w.Template, desired, units, picked))
	if err != nil {
		return capability.Plan{}, err
	}
	changes := make([]any, 0, len(chosen))
	for _, c := range chosen {
		changes = append(changes, c.Says)
	}
	p := capability.Plan{
		Target:      w.Identity(),
		Params:      map[string]any{"revision": revision, "changes": changes, "templateHash": desiredHash},
		Record:      rec,
		ManagedBy:   w.ManagedBy,
		Risk:        "medium",
		BlastRadius: capability.Rollout(w),
		Facts:       partialFacts(units, chosen),
		Mark:        desiredHash,
		State:       partialLive{Workload: w, Desired: desired},
	}
	p.Done = Partial{}.Words(t.Name, p.Params, rec).Done
	return p, nil
}

// refused is err, from reading the target, as a refusal when it says why the
// fix no longer applies.
func refused(err error, revision int64) error {
	if errors.Is(err, ErrStale) {
		return &capability.Refusal{Reason: Explain(err, revision)}
	}
	return err
}

// latestRollout is the revision before w's current one, which the latest
// rollout changed: the one a partial rollback goes back to, and must be
// revision.
func latestRollout(ctx context.Context, w *workload.Workload, revision int64) (workload.Revision, error) {
	if w.Revision == revision {
		return workload.Revision{}, &capability.Refusal{Reason: Explain(ErrAlreadyCurrent, revision), NoOp: true}
	}
	revisions, err := w.Revisions(ctx)
	if err != nil {
		return workload.Revision{}, err
	}
	var earlier *workload.Revision
	found := false
	for i := range revisions {
		r := &revisions[i]
		found = found || r.Number == revision
		if r.Number > 0 && r.Number < w.Revision && (earlier == nil || r.Number > earlier.Number) {
			earlier = r
		}
	}
	switch {
	case !found:
		return workload.Revision{}, &capability.Refusal{Reason: Explain(ErrNoSuchRevision, revision)}
	case earlier == nil || earlier.Number != revision:
		return workload.Revision{}, &capability.Refusal{Reason: fmt.Sprintf(
			"A partial rollback takes changes of the latest rollout back to the revision before it, and revision %d isn't the one before revision %d.", revision, w.Revision)}
	}
	return *earlier, nil
}

// choose is each change of units that named names, in the units' order. A
// change may be named as the rollout history says it, with or without a
// capital and a full stop, by what it is about, by its key, or cut short as
// a long one is in the evidence. One that is none of them is refused,
// naming the rollout's changes.
func choose(units []workload.Change, named []any, current int64) ([]workload.Change, error) {
	picked := make([]bool, len(units))
	for _, n := range named {
		s, _ := n.(string)
		i := slices.IndexFunc(units, func(u workload.Change) bool { return matches(u, s) })
		if i < 0 {
			says := make([]string, 0, len(units))
			for _, u := range units {
				says = append(says, fmt.Sprintf("%q", u.Says))
			}
			return nil, &capability.Refusal{Reason: fmt.Sprintf("%q isn't one of the changes revision %d made to what the pods run. They are: %s.",
				s, current, strings.Join(says[:min(len(says), 20)], ", "))}
		}
		picked[i] = true
	}
	var out []workload.Change
	for i, u := range units {
		if picked[i] {
			out = append(out, u)
		}
	}
	return out, nil
}

// cutShort is the shortest part of a long change that names it, as the
// evidence cuts it.
const cutShort = 64

func matches(u workload.Change, s string) bool {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	if s == "" {
		return false
	}
	lower := strings.ToLower(s[:1]) + s[1:]
	if lower == u.Says || lower == u.What || s == u.Key {
		return true
	}
	return len(s) >= cutShort && strings.HasPrefix(u.Says, strings.TrimSuffix(lower, "…"))
}

// mounted refuses a template that mounts a volume it no longer has, as
// rolling back a new volume without the mounts of it would leave.
func mounted(t corev1.PodTemplateSpec, units []workload.Change) error {
	has := map[string]bool{}
	for _, v := range t.Spec.Volumes {
		has[v.Name] = true
	}
	for _, c := range slices.Concat(t.Spec.InitContainers, t.Spec.Containers) {
		for _, m := range c.VolumeMounts {
			if has[m.Name] {
				continue
			}
			with := "the volume " + m.Name
			if i := slices.IndexFunc(units, func(u workload.Change) bool { return u.Key == "volume."+m.Name }); i >= 0 {
				with = units[i].What
			}
			return &capability.Refusal{Reason: fmt.Sprintf(
				"Rolling back only these changes leaves container %s mounting the volume %s, which the pod would no longer have. Roll back %s and the volume mounts of container %s together.",
				c.Name, m.Name, with, c.Name)}
		}
	}
	return nil
}

// partialFacts are what policy sees of a partial rollback: each change it
// rolls back, by its key, as rolled_back; image, rolled_back when it rolls an
// image back, kept when the rollout changed one it keeps, else unchanged; and
// all, true when it rolls back every change the rollout made to what the pods
// run, which a rollback would do too.
func partialFacts(units, chosen []workload.Change) map[string]string {
	facts := map[string]string{"image": "unchanged", "all": fmt.Sprint(len(chosen) == len(units))}
	for _, u := range units {
		if u.Image && facts["image"] == "unchanged" {
			facts["image"] = "kept"
		}
	}
	for _, c := range chosen {
		facts[c.Key] = "rolled_back"
		if c.Image {
			facts["image"] = "rolled_back"
		}
	}
	return facts
}

// Apply writes the template Plan worked out.
func (Partial) Apply(ctx context.Context, c client.Client, p capability.Plan, dryRun bool) (int64, error) {
	live, ok := p.State.(partialLive)
	if !ok {
		return 0, errors.New("not a partial rollback plan")
	}
	return live.Workload.SetTemplate(ctx, c, live.Desired, dryRun)
}

// Undo is a rollback to the revision the partial rollback replaced, by the
// hash of its template, as a rollback's is.
func (Partial) Undo(ctx context.Context, r client.Reader, t incident.TargetRef, raw json.RawMessage) (string, map[string]any, error) {
	rec, _ := readPartial(raw)
	return undo(ctx, r, t, rec.From)
}

// Changes lists what the partial rollback changes in the pod template, as a
// rollback's are listed, from its record.
func (Partial) Changes(raw json.RawMessage) ([]capability.Change, error) {
	rec, err := readPartial(raw)
	if err != nil {
		return nil, err
	}
	return rec.Changes, nil
}

// Record is the record of a partial rollback from the template before, as
// an earlier version of Themis stored it with the earlier revision's, to
// the template after, rolling back the changes params name to the revision
// they name.
func (Partial) Record(params map[string]any, before, after json.RawMessage) (json.RawMessage, error) {
	var b partialBefore
	if err := json.Unmarshal(before, &b); err != nil {
		return nil, fmt.Errorf("templates before the fix: %w", err)
	}
	var desired corev1.PodTemplateSpec
	if err := json.Unmarshal(after, &desired); err != nil {
		return nil, fmt.Errorf("template the fix writes: %w", err)
	}
	from, err := hashWithout(b.Template)
	if err != nil {
		return nil, err
	}
	earlier, err := hashWithout(b.Earlier)
	if err != nil {
		return nil, err
	}
	revision, _ := RevisionParam(params)
	named, _ := changesParam(params)
	units, _ := workload.Changes(b.Earlier, b.Template)
	rec := newPartialRecord(revisionRef{TemplateHash: from}, revisionRef{Number: revision, TemplateHash: earlier}, b.Template, desired, units, chosenBy(named))
	return json.Marshal(rec)
}

// Words name a partial rollback by the changes it rolls back and the
// revision they go back to: "Roll back checkout's environment variable MODE
// to revision 1". Worked out against the cluster, they also say what it
// keeps: "Keeps the rollout's new image."
func (Partial) Words(target string, params map[string]any, raw json.RawMessage) capability.Words {
	to := "an earlier revision"
	if revision, ok := RevisionParam(params); ok {
		to = fmt.Sprintf("revision %d", revision)
	}
	named, _ := changesParam(params)
	what := fmt.Sprintf("%d of %s changes", len(named), possessive(target))
	if len(named) == 1 {
		what = "one of " + possessive(target) + " changes"
	}
	note := ""
	if rec, err := readPartial(raw); err == nil {
		var chosen, kept []rolledOut
		for _, u := range rec.Rollout {
			if u.Chosen {
				chosen = append(chosen, u)
			} else {
				kept = append(kept, u)
			}
		}
		if len(chosen) > 0 {
			what = phrase(target, chosen, rec.Only)
			note = keeps(kept, to)
		}
	}
	roll := capability.Verb{Base: "Roll", Doing: "Rolling", Done: "Rolled"}
	w := capability.Say("partial rollback", roll, "back "+what+" to "+to, capability.PodsStayReady)
	w.Note = note
	return w
}

// phrase names the changes chosen in a sentence: up to three by what they
// are about, "checkout's environment variable MODE and image", and more by
// their number. only says the template has one container.
func phrase(target string, chosen []rolledOut, only bool) string {
	if len(chosen) > 3 {
		return fmt.Sprintf("%d of %s changes", len(chosen), possessive(target))
	}
	container := chosen[0].Container
	same := container != ""
	for _, c := range chosen {
		same = same && c.Container == container
	}
	nouns := make([]string, 0, len(chosen))
	for _, c := range chosen {
		nouns = append(nouns, c.Noun)
	}
	if same {
		return capability.Of(target, []string{container}, only, capability.And(nouns))
	}
	parts := make([]string, 0, len(chosen))
	for _, c := range chosen {
		if c.Container != "" {
			parts = append(parts, capability.Of(target, []string{c.Container}, only, c.Noun))
		} else {
			parts = append(parts, possessive(target)+" "+c.Noun)
		}
	}
	return capability.And(parts)
}

// keeps says what a partial rollback keeps of the rollout, or that it keeps
// nothing, which a rollback to the revision named to does too.
func keeps(kept []rolledOut, to string) string {
	images := 0
	for _, k := range kept {
		if k.Image {
			images++
		}
	}
	switch {
	case len(kept) == 0:
		return "Rolls back every change of the rollout to what the pods run, the same as a rollback to " + to + "."
	case len(kept) == 1 && images == 1:
		return "Keeps the rollout's new image."
	case len(kept) == 1:
		return "Keeps the rollout's change to " + kept[0].What + "."
	case images == 1:
		return fmt.Sprintf("Keeps the rollout's other %d changes, including the new image.", len(kept))
	case images > 1:
		return fmt.Sprintf("Keeps the rollout's other %d changes, including the new images.", len(kept))
	default:
		return fmt.Sprintf("Keeps the rollout's other %d changes.", len(kept))
	}
}

// possessive is name's: checkout's, and payments' for a name that ends in s.
func possessive(name string) string {
	if strings.HasSuffix(name, "s") {
		return name + "'"
	}
	return name + "'s"
}

// Mark is the hash of the target's live pod template: a partial rollback
// leaves the template it worked out.
func (Partial) Mark(ctx context.Context, r client.Reader, t incident.TargetRef) (string, error) {
	return Capability{}.Mark(ctx, r, t)
}

// Check reads the rollout of the template the partial rollback wrote.
func (Partial) Check(ctx context.Context, r client.Reader, t incident.TargetRef, mark string, from json.RawMessage, since time.Time) (capability.Reading, error) {
	return capability.ReadRollout(ctx, r, t, mark, from, since)
}

// Describe gives the image digests the pods run, before and after, of each
// container whose image the partial rollback rolls back.
func (Partial) Describe(ctx context.Context, _ client.Reader, p capability.Plan) (map[string]any, error) {
	live, ok := p.State.(partialLive)
	if !ok {
		return nil, errors.New("not a partial rollback plan")
	}
	return images(ctx, live.Workload, live.Workload.Template, live.Desired)
}
