package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/fix"
	"github.com/hzerrad/themis/internal/incident"
)

// Fix is deployment.rollback-to-revision as a fix.Fix.
type Fix struct{}

var _ fix.Fix = Fix{}

func (Fix) Name() string    { return Name }
func (Fix) Version() string { return Version }

func (Fix) Description() string {
	return "Roll the Deployment back to the pod template of an earlier revision, as kubectl rollout undo does. revision is that revision's number, from the rollout history."
}

var schema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["revision"],` +
	`"properties":{"revision":{"type":"integer","minimum":1,"description":"The earlier revision to roll back to."}}}`)

func (Fix) Schema() json.RawMessage { return schema }

// Validate needs a revision: a positive whole number, and nothing else.
func (Fix) Validate(params map[string]any) (map[string]any, error) {
	revision, ok := RevisionParam(params)
	if !ok || len(params) != 1 {
		return nil, fmt.Errorf("%w: a rollback needs only revision, a positive whole number", fix.ErrInvalidParams)
	}
	return map[string]any{"revision": revision}, nil
}

// Plan reads the target and the template of the revision params name.
func (Fix) Plan(ctx context.Context, r client.Reader, t incident.TargetRef, params map[string]any) (fix.Plan, error) {
	revision, ok := RevisionParam(params)
	if !ok {
		return fix.Plan{}, fix.ErrInvalidParams
	}
	live, err := Read(ctx, r, t, revision)
	if errors.Is(err, ErrStale) || errors.Is(err, ErrNoSuchRevision) || errors.Is(err, ErrAlreadyCurrent) {
		return fix.Plan{}, &fix.Refusal{Reason: Explain(err, revision), NoOp: errors.Is(err, ErrAlreadyCurrent)}
	}
	if err != nil {
		return fix.Plan{}, err
	}
	before, err := json.Marshal(live.PreState)
	if err != nil {
		return fix.Plan{}, err
	}
	after, err := json.Marshal(live.Desired)
	if err != nil {
		return fix.Plan{}, err
	}
	return fix.Plan{
		Target: live.Target, Params: Params(revision, live.DesiredHash),
		Before: before, After: after,
		ManagedBy:   live.ManagedBy,
		Risk:        "medium",
		BlastRadius: map[string]any{"replicas": int64(live.Replicas)},
		Done:        fmt.Sprintf("Rolled back to revision %d.", revision),
		State:       live,
	}, nil
}

// Apply writes the revision's template.
func (Fix) Apply(ctx context.Context, c client.Client, p fix.Plan, dryRun bool) (int64, error) {
	live, ok := p.State.(Live)
	if !ok {
		return 0, errors.New("not a rollback plan")
	}
	return Apply(ctx, c, live, dryRun)
}

// Undo is a rollback to the revision whose template was there before.
func (Fix) Undo(ctx context.Context, r client.Reader, t incident.TargetRef, before json.RawMessage) (string, map[string]any, error) {
	var template corev1.PodTemplateSpec
	if err := json.Unmarshal(before, &template); err != nil {
		return "", nil, fmt.Errorf("template before the fix: %w", err)
	}
	revision, err := RevisionOf(ctx, r, t, template)
	if errors.Is(err, ErrNoSuchRevision) {
		return "", nil, &fix.Refusal{Reason: "Themis couldn't put the workload back: Kubernetes no longer keeps the version from before it acted."}
	}
	if errors.Is(err, ErrStale) {
		return "", nil, &fix.Refusal{Reason: "The workload was deleted or replaced, so there is nothing to put back."}
	}
	if err != nil {
		return "", nil, err
	}
	return Name, map[string]any{"revision": revision}, nil
}

// Changes lists what writing after over before changes in the pod template.
func (Fix) Changes(before, after json.RawMessage) ([]fix.Change, error) {
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
