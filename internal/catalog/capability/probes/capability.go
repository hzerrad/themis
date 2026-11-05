package probes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// Capability is deployment.set-probes as a capability.Capability.
type Capability struct{}

var _ capability.Capability = Capability{}

func (Capability) Name() string    { return Name }
func (Capability) Version() string { return Version }
func (Capability) Noun() string    { return "a probe change" }

func (Capability) Description() string {
	return "Change the timing of a container's existing liveness, readiness or startup probes: " +
		"initialDelaySeconds, periodSeconds, timeoutSeconds, failureThreshold, and successThreshold for readiness. " +
		"Give only the fields to change, with the values the evidence supports. " +
		"It can't add or remove a probe, or change what the probe calls."
}

var schema = func() json.RawMessage {
	probe := func(kind string) map[string]any {
		props := map[string]any{}
		for _, f := range fieldsOf(kind) {
			props[f] = map[string]any{"type": "integer", "minimum": minimum(f), "maximum": math.MaxInt32}
		}
		return map[string]any{
			"type": "object", "additionalProperties": false, "minProperties": 1, "properties": props,
			"description": "The " + kind + " probe's fields to change; the others stay as they are.",
		}
	}
	b, err := json.Marshal(map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"container"}, "minProperties": 2,
		"properties": map[string]any{
			"container": map[string]any{"type": "string", "minLength": 1, "description": "The container whose probes to change."},
			"liveness":  probe("liveness"),
			"readiness": probe("readiness"),
			"startup":   probe("startup"),
		},
	})
	if err != nil {
		panic(err)
	}
	return b
}()

func (Capability) Schema() json.RawMessage { return schema }

// Validate needs a container and at least one probe's fields, each a whole
// number Kubernetes accepts.
func (Capability) Validate(params map[string]any) (map[string]any, error) {
	r, err := parse(params, true)
	if err != nil {
		return nil, err
	}
	return r.params(), nil
}

// state is what Apply writes: the template planned, over the workload read.
type state struct {
	workload *workload.Workload
	desired  corev1.PodTemplateSpec
}

// Plan reads the target and sets the fields params name on its container's
// probes, which must exist.
func (Capability) Plan(ctx context.Context, r client.Reader, t incident.TargetRef, params map[string]any) (capability.Plan, error) {
	req, err := parse(params, false)
	if err != nil {
		return capability.Plan{}, err
	}
	w, err := workload.Read(ctx, r, t)
	if workload.Lost(err) {
		return capability.Plan{}, &capability.Refusal{Reason: lost(err)}
	}
	if err != nil {
		return capability.Plan{}, err
	}
	desired := *w.Template.DeepCopy()
	c := container(&desired, req.container)
	if c == nil {
		return capability.Plan{}, &capability.Refusal{Reason: fmt.Sprintf("The workload has no container named %s.", req.container)}
	}
	for _, kind := range kinds {
		values, ok := req.set[kind]
		if !ok {
			continue
		}
		p := probe(c, kind)
		if p == nil {
			return capability.Plan{}, &capability.Refusal{Reason: fmt.Sprintf(
				"Container %s has no %s probe. Themis only adjusts probes that exist; adding one is a change for a person.", req.container, kind)}
		}
		for f, v := range values {
			*field(p, f) = v
		}
	}
	edits := diff(w.Template, desired)
	if len(edits) == 0 {
		return capability.Plan{}, &capability.Refusal{NoOp: true, Reason: "The probes already have these settings, so there's nothing to change."}
	}
	mark, err := workload.TemplateHash(desired)
	if err != nil {
		return capability.Plan{}, err
	}
	before, err := json.Marshal(w.Template)
	if err != nil {
		return capability.Plan{}, err
	}
	after, err := json.Marshal(desired)
	if err != nil {
		return capability.Plan{}, err
	}
	return capability.Plan{
		Target: w.Identity(), Params: req.params(),
		Before: before, After: after,
		ManagedBy:   w.ManagedBy,
		Risk:        "medium",
		BlastRadius: map[string]any{"replicas": int64(w.Replicas.Desired)},
		Facts:       facts(edits, req.container),
		Done:        done(edits, req.container),
		Mark:        mark,
		State:       state{workload: w, desired: desired},
	}, nil
}

// lost says in words why a fix for a lost target can't go ahead.
func lost(err error) string {
	switch {
	case errors.Is(err, workload.ErrGone):
		return "The workload was deleted, so the fix no longer applies."
	case errors.Is(err, workload.ErrReplaced):
		return "The workload was replaced by a new one with the same name, so the fix no longer applies."
	default:
		return "Themis can only change the probes of Deployments."
	}
}

// Apply writes the planned template with the resourceVersion read as
// precondition, so a write since the read conflicts.
func (Capability) Apply(ctx context.Context, c client.Client, p capability.Plan, dryRun bool) (int64, error) {
	s, ok := p.State.(state)
	if !ok {
		return 0, errors.New("not a probe plan")
	}
	return s.workload.SetTemplate(ctx, c, s.desired, dryRun)
}

// Undo sets back the probe fields that differ between the template before
// the fix and the live one, on the one container the fix changed.
func (Capability) Undo(ctx context.Context, r client.Reader, t incident.TargetRef, before json.RawMessage) (string, map[string]any, error) {
	var template corev1.PodTemplateSpec
	if err := json.Unmarshal(before, &template); err != nil {
		return "", nil, fmt.Errorf("template before the fix: %w", err)
	}
	w, err := workload.Read(ctx, r, t)
	if errors.Is(err, workload.ErrGone) {
		return "", nil, &capability.Refusal{Reason: "The workload was deleted, so there is nothing to put back."}
	}
	if errors.Is(err, workload.ErrReplaced) {
		return "", nil, &capability.Refusal{Reason: "The workload was replaced by a new one with the same name, so there is nothing to put back."}
	}
	if err != nil {
		return "", nil, err
	}
	edits := diff(w.Template, template)
	if len(edits) == 0 {
		hash, err := workload.TemplateHash(template)
		if err != nil {
			return "", nil, err
		}
		if hash == w.TemplateHash {
			return "", nil, &capability.Refusal{NoOp: true, Reason: "The probes are already as they were before Themis acted."}
		}
		return "", nil, &capability.Refusal{Reason: "The workload changed after Themis's fix, so Themis left it as it is."}
	}
	req := request{container: edits[0].container, set: map[string]map[string]int32{}}
	for _, e := range edits {
		if e.container != req.container {
			return "", nil, &capability.Refusal{Reason: "The probes of more than one container changed since Themis acted, so Themis left them as they are."}
		}
		if req.set[e.kind] == nil {
			req.set[e.kind] = map[string]int32{}
		}
		req.set[e.kind][e.field] = e.after
	}
	return Name, req.params(), nil
}

// Changes lists each probe field writing after over before changes.
func (Capability) Changes(before, after json.RawMessage) ([]capability.Change, error) {
	var pre, desired corev1.PodTemplateSpec
	if err := json.Unmarshal(before, &pre); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(after, &desired); err != nil {
		return nil, err
	}
	return changes(diff(pre, desired)), nil
}

// Mark is the hash of the target's live pod template: the fix leaves the
// template it planned.
func (Capability) Mark(ctx context.Context, r client.Reader, t incident.TargetRef) (string, error) {
	w, err := workload.Read(ctx, r, t)
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

// Check reads the current revision's pods. The probes are right when the
// rollout is complete, every replica is ready, and no container has
// restarted since checking began: a liveness probe that still fails a
// working container restarts it.
func (Capability) Check(ctx context.Context, r client.Reader, t incident.TargetRef, mark string, from json.RawMessage, since time.Time) (capability.Reading, error) {
	w, err := workload.Read(ctx, r, t)
	if workload.Lost(err) {
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
	allReady := s.RolloutComplete && s.Ready >= s.Desired
	health := capability.Unchanged
	switch {
	case allReady && restarts == 0:
		health = capability.Healthy
	case s.Ready < base.Ready:
		health = capability.Worse
	case allReady:
		health = capability.Improved
	}
	msg := fmt.Sprintf("%d of %d pods are ready", s.Ready, s.Desired)
	if s.Ready < base.Ready {
		msg += fmt.Sprintf(", down from %d", base.Ready)
	}
	switch {
	case restarts == 1:
		msg += "; containers restarted once"
	case restarts > 1:
		msg += fmt.Sprintf("; containers restarted %d times", restarts)
	}
	return capability.Reading{
		Changed: w.TemplateHash != mark, Applied: s.Updated, Health: health,
		Facts: msg + ".", Start: from,
	}, nil
}
