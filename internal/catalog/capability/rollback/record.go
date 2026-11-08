package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// revisionRef is a revision as a record names it: its number, the
// ReplicaSet that holds it, and the hash of its pod template. Never the
// template: its environment can hold secrets.
type revisionRef struct {
	Number       int64  `json:"revision,omitempty"`
	ReplicaSet   string `json:"replicaSet,omitempty"`
	UID          string `json:"uid,omitempty"`
	TemplateHash string `json:"templateHash"`
}

// refOf names r, whose template hashes to hash.
func refOf(r workload.Revision, hash string) revisionRef {
	out := revisionRef{Number: r.Number, TemplateHash: hash}
	if r.Object != nil {
		out.ReplicaSet, out.UID = r.Object.Name, string(r.Object.UID)
	}
	return out
}

// current names w's current revision, with the ReplicaSet that holds it
// once Kubernetes made one.
func current(ctx context.Context, w *workload.Workload) (revisionRef, error) {
	out := revisionRef{Number: w.Revision, TemplateHash: w.TemplateHash}
	if w.Revision == 0 {
		return out, nil
	}
	r, err := w.RevisionAt(ctx, w.Revision)
	switch {
	case errors.Is(err, workload.ErrNoRevision):
		return out, nil
	case err != nil:
		return revisionRef{}, err
	}
	return refOf(r, w.TemplateHash), nil
}

// record is what Themis keeps of a rollback: the revision it replaces,
// which putting it back rolls back to, the revision it restores, and what
// it changes as the approver is shown it, values that look secret withheld.
type record struct {
	From    revisionRef         `json:"from"`
	To      revisionRef         `json:"to"`
	Changes []capability.Change `json:"changes"`
	// Undoes counts the changes to what the pods run the rollback undoes,
	// and Images those of them to an image.
	Undoes int `json:"undoes,omitempty"`
	Images int `json:"images,omitempty"`
}

// newRecord is the record of a rollback from pre, the template of from, to
// desired, the template of to.
func newRecord(from, to revisionRef, pre, desired corev1.PodTemplateSpec) record {
	undone, _ := workload.Changes(desired, pre)
	images := 0
	for _, c := range undone {
		if c.Image {
			images++
		}
	}
	return record{From: from, To: to, Changes: Changes(pre, desired), Undoes: len(undone), Images: images}
}

// readRecord reads a rollback's record: ErrNoRecord when it names no
// revision it replaced.
func readRecord(raw json.RawMessage) (record, error) {
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return record{}, fmt.Errorf("%w: %w", capability.ErrNoRecord, err)
	}
	if r.From.TemplateHash == "" {
		return record{}, capability.ErrNoRecord
	}
	return r, nil
}

// partialRecord is what Themis keeps of a partial rollback: the revision it
// replaces, which putting it back rolls back to, the earlier revision its
// changes go back to, what it changes as the approver is shown it, the
// latest rollout's changes by what they are about, and how kubectl makes it.
type partialRecord struct {
	From    revisionRef         `json:"from"`
	Earlier revisionRef         `json:"earlier"`
	Changes []capability.Change `json:"changes"`
	// Rollout are the latest rollout's changes to what the pods run, Chosen
	// marking those the partial rollback rolls back.
	Rollout []rolledOut `json:"rollout"`
	// Only says the pod template has one container.
	Only bool `json:"only,omitempty"`
	// Set are the kubectl set commands that make the change: none when
	// kubectl set can't.
	Set []command `json:"set,omitempty"`
}

// rolledOut is one change of the latest rollout, named by what it is about,
// never by its values.
type rolledOut struct {
	Key       string `json:"key"`
	What      string `json:"what"`
	Noun      string `json:"noun"`
	Container string `json:"container,omitempty"`
	Image     bool   `json:"image,omitempty"`
	Chosen    bool   `json:"chosen,omitempty"`
}

// command is a kubectl command on the workload, by its verb and arguments,
// each already one shell word.
type command struct {
	Verb string   `json:"verb"`
	Args []string `json:"args"`
}

// newPartialRecord is the record of a partial rollback from pre, the
// template of from, to desired, rolling back chosen of the rollout's units
// to earlier.
func newPartialRecord(from, earlier revisionRef, pre, desired corev1.PodTemplateSpec, units []workload.Change, chosen func(workload.Change) bool) partialRecord {
	rollout := make([]rolledOut, 0, len(units))
	for _, u := range units {
		rollout = append(rollout, rolledOut{Key: u.Key, What: u.What, Noun: u.Noun, Container: u.Container, Image: u.Image, Chosen: chosen(u)})
	}
	return partialRecord{
		From: from, Earlier: earlier, Changes: Changes(pre, desired), Rollout: rollout,
		Only: len(pre.Spec.Containers) == 1, Set: setCommands(pre, desired),
	}
}

// readPartial reads a partial rollback's record: ErrNoRecord when it names
// no revision it replaced.
func readPartial(raw json.RawMessage) (partialRecord, error) {
	var r partialRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return partialRecord{}, fmt.Errorf("%w: %w", capability.ErrNoRecord, err)
	}
	if r.From.TemplateHash == "" {
		return partialRecord{}, capability.ErrNoRecord
	}
	return r, nil
}

// undo is a rollback to from, the revision a fix replaced, found by the hash
// of its template, so the restore writes exactly the template from before
// the fix.
func undo(ctx context.Context, r client.Reader, t incident.TargetRef, from revisionRef) (string, map[string]any, error) {
	if from.TemplateHash == "" {
		return "", nil, &capability.Refusal{Reason: "Not restored. Themis kept no record of the revision from before it acted. The fix stays in place."}
	}
	revision, err := RevisionWith(ctx, r, t, from.TemplateHash)
	if errors.Is(err, ErrNoSuchRevision) {
		return "", nil, &capability.Refusal{Reason: "Not restored. Kubernetes no longer keeps the revision from before Themis acted. The fix stays in place."}
	}
	if errors.Is(err, workload.ErrGone) {
		return "", nil, &capability.Refusal{Reason: "Not restored. The workload was deleted."}
	}
	if errors.Is(err, ErrStale) {
		return "", nil, &capability.Refusal{Reason: "Not restored. The workload was replaced by a new one with the same name."}
	}
	if err != nil {
		return "", nil, err
	}
	return Name, map[string]any{"revision": revision}, nil
}

// RevisionWith returns the revision of the target's ReplicaSet whose pod
// template hashes to hash, such as the template a fix replaced: rolling
// back to it puts the workload back as it was.
func RevisionWith(ctx context.Context, c client.Reader, t incident.TargetRef, hash string) (int64, error) {
	w, err := read(ctx, c, t)
	if err != nil {
		return 0, err
	}
	revisions, err := w.Revisions(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range revisions {
		h, err := TemplateHash(r.Template)
		if err != nil {
			return 0, err
		}
		if h == hash {
			if r.Number == 0 {
				return 0, fmt.Errorf("revision of ReplicaSet %s is not a number", r.Object.Name)
			}
			return r.Number, nil
		}
	}
	return 0, fmt.Errorf("%w: none has the template to put back", ErrNoSuchRevision)
}

// hashWithout hashes t without Kubernetes' own per-revision label, as a
// revision's template is hashed.
func hashWithout(t corev1.PodTemplateSpec) (string, error) {
	t = *t.DeepCopy()
	delete(t.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	return TemplateHash(t)
}

// templates reads the two pod templates an earlier version of Themis stored
// for a change.
func templates(before, after json.RawMessage) (pre, desired corev1.PodTemplateSpec, err error) {
	if err := json.Unmarshal(before, &pre); err != nil {
		return pre, desired, fmt.Errorf("template before the fix: %w", err)
	}
	if err := json.Unmarshal(after, &desired); err != nil {
		return pre, desired, fmt.Errorf("template the fix writes: %w", err)
	}
	return pre, desired, nil
}

// chosenBy reports whether a change is one of those named.
func chosenBy(named []any) func(workload.Change) bool {
	return func(u workload.Change) bool {
		return slices.ContainsFunc(named, func(n any) bool { s, _ := n.(string); return matches(u, s) })
	}
}
