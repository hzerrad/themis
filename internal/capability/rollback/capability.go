package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/capability"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// Capability is deployment.rollback-to-revision as a capability.Capability.
type Capability struct{}

var _ capability.Capability = Capability{}

func (Capability) Name() string    { return Name }
func (Capability) Version() string { return Version }

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
	before, err := json.Marshal(live.PreState)
	if err != nil {
		return capability.Plan{}, err
	}
	after, err := json.Marshal(live.Desired)
	if err != nil {
		return capability.Plan{}, err
	}
	return capability.Plan{
		Target: live.Target, Params: Params(revision, live.DesiredHash),
		Before: before, After: after,
		ManagedBy:   live.ManagedBy,
		Risk:        "medium",
		BlastRadius: map[string]any{"replicas": int64(live.Replicas)},
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

// Undo is a rollback to the revision whose template was there before.
func (Capability) Undo(ctx context.Context, r client.Reader, t incident.TargetRef, before json.RawMessage) (string, map[string]any, error) {
	var template corev1.PodTemplateSpec
	if err := json.Unmarshal(before, &template); err != nil {
		return "", nil, fmt.Errorf("template before the fix: %w", err)
	}
	revision, err := RevisionOf(ctx, r, t, template)
	if errors.Is(err, ErrNoSuchRevision) {
		return "", nil, &capability.Refusal{Reason: "Themis couldn't put the workload back: Kubernetes no longer keeps the version from before it acted."}
	}
	if errors.Is(err, workload.ErrGone) {
		return "", nil, &capability.Refusal{Reason: "The workload was deleted, so there is nothing to put back."}
	}
	if errors.Is(err, ErrStale) {
		return "", nil, &capability.Refusal{Reason: "The workload was replaced by a new one with the same name, so there is nothing to put back."}
	}
	if err != nil {
		return "", nil, err
	}
	return Name, map[string]any{"revision": revision}, nil
}

// Changes lists what writing after over before changes in the pod template.
func (Capability) Changes(before, after json.RawMessage) ([]capability.Change, error) {
	var pre, desired corev1.PodTemplateSpec
	if err := json.Unmarshal(before, &pre); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(after, &desired); err != nil {
		return nil, err
	}
	return Changes(pre, desired), nil
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

// start is how the workload was when checking began.
type start struct {
	Ready    int32 `json:"ready"`
	Restarts int32 `json:"restarts"`
}

// Check reads the current revision's pods. A rollback cured its symptom when
// the rollout is complete, every replica is ready, and no container has
// restarted or run out of memory since.
func (Capability) Check(ctx context.Context, r client.Reader, t incident.TargetRef, mark string, from json.RawMessage, since time.Time) (capability.Reading, error) {
	w, err := workload.Read(ctx, r, t)
	if errors.Is(err, workload.ErrGone) || errors.Is(err, workload.ErrReplaced) {
		return capability.Reading{Changed: true, Health: capability.Unchanged, Facts: err.Error() + ".", Start: from}, nil
	}
	if err != nil {
		return capability.Reading{}, err
	}
	s, err := w.State(ctx, since)
	if err != nil {
		return capability.Reading{}, err
	}
	base := start{Ready: s.Ready, Restarts: s.Restarts}
	if from == nil {
		if from, err = json.Marshal(base); err != nil {
			return capability.Reading{}, err
		}
	} else if err := json.Unmarshal(from, &base); err != nil {
		return capability.Reading{}, fmt.Errorf("start of the check: %w", err)
	}
	restarts := max(s.Restarts-base.Restarts, 0)
	allReady := s.RolloutComplete && s.Ready >= s.Desired && s.OOMKills == 0
	health := capability.Unchanged
	switch {
	case allReady && restarts == 0:
		health = capability.Healthy
	case s.Ready < base.Ready:
		health = capability.Worse
	case allReady:
		health = capability.Improved
	}
	return capability.Reading{
		Changed: w.TemplateHash != mark, Applied: s.Updated, Health: health,
		Facts: facts(s, base, restarts), Start: from,
	}, nil
}

// facts says what the pods are doing, in plain words.
func facts(s workload.State, base start, restarts int32) string {
	msg := fmt.Sprintf("%d of %d pods are ready", s.Ready, s.Desired)
	if s.Ready < base.Ready {
		msg += fmt.Sprintf(", down from %d", base.Ready)
	}
	if restarts > 0 {
		msg += "; containers restarted " + times(int(restarts))
	}
	if s.OOMKills > 0 {
		msg += "; containers ran out of memory " + times(s.OOMKills)
	}
	return msg + "."
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}
