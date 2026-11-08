// Package capability is what every fix Themis can carry out provides: how to work
// it out against the live cluster, apply it, show it and undo it. Governance,
// the executor and restore go through it, so a new fix is one implementation
// and no change elsewhere.
package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

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
	// Noun is the fix in a few plain words, such as "a rollback", for what
	// operators read.
	Noun() string

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

	// Undo works out how to put back a change this fix made, from the
	// record Plan kept of it: the fix to use and its parameters. A *Refusal
	// says why it can't be put back.
	Undo(ctx context.Context, r client.Reader, t incident.TargetRef, record json.RawMessage) (string, map[string]any, error)

	// Changes lists what the change does, for the person approving it, from
	// the record Plan kept of it.
	Changes(record json.RawMessage) ([]Change, error)

	// Words name the change params ask for to the workload named target, for
	// the people who decide on it, from the record Plan kept of it. record
	// is nil until the fix is worked out against the cluster: the words then
	// say what params ask for.
	Words(target string, params map[string]any, record json.RawMessage) Words

	// Mark reads, from the live target, the same mark Plan records for the
	// state the fix leaves: a different one means the target no longer has
	// what the fix left.
	Mark(ctx context.Context, r client.Reader, t incident.TargetRef) (string, error)

	// Check reads how the target is doing since the fix was applied, at
	// since, against start: the fix's own record of how things were when
	// checking began, or nil when it begins now. A target deleted or
	// replaced is a Reading with Changed set, not an error.
	Check(ctx context.Context, r client.Reader, t incident.TargetRef, mark string, start json.RawMessage, since time.Time) (Reading, error)
}

// Feasibility is a fix that can tell whether its change could work in the
// cluster as it is, beyond the target itself: whether nodes, quotas and
// limits allow it. Governance asks after Plan and before policy decides. The
// executor doesn't: it may read only the target, and acts on what governance
// decided.
type Feasibility interface {
	// Feasible returns a *Refusal saying why p can't work.
	Feasible(ctx context.Context, r client.Reader, p Plan) error
}

// Commanding is a fix that says how a person makes its change with kubectl,
// for one Themis only recommends.
type Commanding interface {
	// Commands are kubectl commands that make the change params ask for, as
	// record has it, to the workload t: that change exactly, printing no
	// more of a value than Changes shows. None when kubectl can't.
	Commands(t incident.TargetRef, params map[string]any, record json.RawMessage) []string
}

// Templating is a fix whose change writes a new pod template. Record is the
// record Plan keeps of a change from the template before to the one after,
// for params. Themis once stored those two templates whole: the migration
// that removed them keeps this record instead.
type Templating interface {
	Record(params map[string]any, before, after json.RawMessage) (json.RawMessage, error)
}

// Kubectl is a kubectl command on the workload t, such as
// "kubectl -n shop rollout undo deployment/checkout --to-revision=1" for
// Kubectl(t, "rollout undo", "--to-revision=1"). Every argument must
// already be one shell word: see Quote.
func Kubectl(t incident.TargetRef, verb string, args ...string) string {
	return strings.Join(append([]string{"kubectl", "-n", t.Namespace, verb, strings.ToLower(t.Kind) + "/" + t.Name}, args...), " ")
}

// plainWord matches what a shell reads as one word, as it is.
var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// Quote is s as one shell word: as it is when a shell reads it so, else in
// single quotes.
func Quote(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Describing is a fix that tells the approver more of its change than the
// target holds, from what only the core may read, such as the image digests
// the pods run. Governance asks after Plan and adds what it returns to the
// plan's BlastRadius. The executor doesn't, and the approval binds none of it.
type Describing interface {
	Describe(ctx context.Context, r client.Reader, p Plan) (map[string]any, error)
}

// Health is how the target is doing next to when checking began, for the
// symptom the fix is meant to cure.
type Health string

const (
	Healthy   Health = "healthy"
	Improved  Health = "improved"
	Unchanged Health = "unchanged"
	Worse     Health = "worse"
)

// Reading is one look at a target after a fix. The fix judges its symptom;
// verification judges what that means over time.
type Reading struct {
	// Changed is set when the target no longer has what the fix left.
	Changed bool
	// Applied is set once the fix's change is in place everywhere.
	Applied bool
	Health  Health

	// Facts says what was seen, as a sentence for the incident's timeline.
	Facts string

	// Start is the record to compare later readings with: the one passed
	// in, or how things are now when none was.
	Start json.RawMessage
}

// Plan is a fix worked out against the live cluster.
type Plan struct {
	// Target and Params are what the approval binds: a change to either
	// since makes the approval void.
	Target digest.TargetIdentity
	Params map[string]any

	// Record is what Themis keeps of the change: what the approver is shown
	// of it, and what putting it back needs. It holds revisions, ReplicaSets
	// and hashes of pod templates, the values the fix sets when they are
	// never secret, such as memory limits, and the change as Changes lists
	// it, values that look secret withheld: never the pod template itself,
	// whose environment can hold secrets. The digest doesn't bind it.
	Record json.RawMessage

	// ManagedBy names the GitOps tool that owns the target, if any.
	ManagedBy string
	// Risk is what policy sees of the change's danger. BlastRadius is what
	// the approver sees of its reach: for a fix that rolls out new pods,
	// Rollout's replicas, atOnce and deployedBy; for one that changes an
	// image, images, each changed container's image digests before and
	// after, as Describing adds them.
	Risk        string
	BlastRadius map[string]any
	// Facts are what policy sees of the change beyond its name and risk,
	// as change.<key>. Each fix documents its keys; most have none.
	Facts map[string]string

	// Done says what was done, once applied.
	Done string

	// Mark identifies the state the fix leaves the target in.
	Mark string

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
	// Withheld says why Before and After aren't the values themselves, when
	// they aren't: Secret or Summary.
	Withheld string `json:"withheld,omitempty"`
}

// Why a change's values aren't shown.
const (
	// Secret is a value hidden because it looks like a secret.
	Secret = "secret"
	// Summary is a value shown only as changed.
	Summary = "summary"
)

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

// ErrNoRecord means a fix's record says nothing of its change: Themis kept
// none, or couldn't convert the one an earlier version kept.
var ErrNoRecord = errors.New("no record of the change")

// Record is the record the fix name keeps of a change from the pod template
// before to the one after, for params, as Templating works it out.
func (r Registry) Record(name string, params map[string]any, before, after json.RawMessage) (json.RawMessage, error) {
	f, ok := r[name].(Templating)
	if !ok {
		return nil, fmt.Errorf("%s keeps no record of a pod template's change", name)
	}
	return f.Record(params, before, after)
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
