// Package capability is what every fix Themis can carry out provides: how to work
// it out against the live cluster, apply it, show it and undo it. Governance,
// the executor and restore go through it, so a new fix is one implementation
// and no change elsewhere.
package capability

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
)

// Capability is one kind of change Themis can make.
type Capability interface {
	// Name is the capability profiles allow and suggestions name, such as
	// deployment.rollback-to-revision. Version changes when its meaning does.
	Name() string
	Version() string

	// Description says what the fix does, for whoever suggests it. Schema
	// is the JSON Schema of its parameters.
	Description() string
	Schema() json.RawMessage

	// Validate checks params without reading the cluster, and returns them
	// as the fix stores them: JSON numbers decode as float64, for one.
	Validate(params map[string]any) (map[string]any, error)

	// Plan reads the target and works out the change params ask for. A
	// *Refusal says why it can't go ahead; ErrInvalidParams that the
	// parameters make no sense.
	Plan(ctx context.Context, r client.Reader, t incident.TargetRef, params map[string]any) (Plan, error)

	// Apply makes the change Plan worked out, failing if the target changed
	// since, and returns the target's resulting generation.
	Apply(ctx context.Context, c client.Client, p Plan, dryRun bool) (int64, error)

	// Undo works out how to put back a change this fix made, from the state
	// before it: the fix to use and its parameters. A *Refusal says why it
	// can't be put back.
	Undo(ctx context.Context, r client.Reader, t incident.TargetRef, before json.RawMessage) (string, map[string]any, error)

	// Changes lists what the change does, for the person approving it.
	Changes(before, after json.RawMessage) ([]Change, error)
}

// Plan is a fix worked out against the live cluster.
type Plan struct {
	// Target and Params are what the approval binds: a change to either
	// since makes the approval void.
	Target digest.TargetIdentity
	Params map[string]any

	// Before and After are the target's state the fix replaces and writes,
	// for showing the change and putting it back.
	Before json.RawMessage
	After  json.RawMessage

	// ManagedBy names the GitOps tool that owns the target, if any.
	ManagedBy string
	// Risk and BlastRadius are what policy and the approver see.
	Risk        string
	BlastRadius map[string]any

	// Done says what was done, once applied.
	Done string

	// State is the fix's own read of the target, for Apply.
	State any
}

// Refusal is why a fix can't go ahead, in words for the incident's timeline.
type Refusal struct {
	Reason string
	// NoOp means the target already is as the fix would make it.
	NoOp bool
}

func (r *Refusal) Error() string { return r.Reason }

// ErrInvalidParams means a suggestion's parameters make no sense for the fix.
var ErrInvalidParams = errors.New("invalid parameters")

// Change is one thing a fix changes, as the approver sees it.
type Change struct {
	Container string `json:"container"`
	Field     string `json:"field"`
	Before    string `json:"before"`
	After     string `json:"after"`
}

// Registry is the fixes Themis can carry out, by name.
type Registry map[string]Capability

// NewRegistry registers fixes by their names.
func NewRegistry(capabilities ...Capability) Registry {
	r := Registry{}
	for _, f := range capabilities {
		r[f.Name()] = f
	}
	return r
}

// Names lists the registered fixes, sorted.
func (r Registry) Names() []string {
	return slices.Sorted(maps.Keys(r))
}

// Fields are the digest fields of p, made by f, for an incident. restores
// names the action an undo puts back; empty for a fix of its own.
func Fields(incidentID string, f Capability, p Plan, restores, policyID, policyDigest string) digest.ActionFields {
	params := maps.Clone(p.Params)
	if restores != "" {
		params["restores"] = restores
	}
	return digest.ActionFields{
		IncidentID:        incidentID,
		Capability:        f.Name(),
		CapabilityVersion: f.Version(),
		Target:            p.Target,
		Params:            params,
		PolicyID:          policyID,
		PolicyDigest:      policyDigest,
		Constraints:       map[string]any{},
	}
}
