// Package rollback is the deployment.rollback-to-revision capability: it
// restores the pod template of a prior ReplicaSet, as kubectl rollout undo
// does. Governance and the executor both read live state through it, so they
// bind and check the same identity. What Themis keeps of a rollback names
// its revisions by ReplicaSet and template hash: the executor reads the
// template from the ReplicaSet again, and writes it only when its hash is
// the one approved.
package rollback

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

const (
	Name    = "deployment.rollback-to-revision"
	Version = "1"
)

var (
	// ErrStale means the target is gone or is another object now. The
	// workload error it wraps says which.
	ErrStale          = errors.New("target changed")
	ErrNoSuchRevision = errors.New("no ReplicaSet has the revision")
	ErrAlreadyCurrent = errors.New("the revision is already current")
)

// Explain says in words why a rollback to revision cannot go ahead, for err
// one of ErrStale, ErrNoSuchRevision and ErrAlreadyCurrent.
func Explain(err error, revision int64) string {
	switch {
	case errors.Is(err, workload.ErrGone):
		return "The workload was deleted, so the fix no longer applies."
	case errors.Is(err, workload.ErrReplaced):
		return "The workload was replaced by a new one with the same name, so the fix no longer applies."
	case errors.Is(err, workload.ErrUnsupported):
		return "Themis can only roll back Deployments."
	case errors.Is(err, ErrNoSuchRevision):
		return fmt.Sprintf("Revision %d no longer exists, so Themis can't roll back to it.", revision)
	case errors.Is(err, ErrAlreadyCurrent):
		return fmt.Sprintf("Nothing to roll back. The workload already runs revision %d.", revision)
	default:
		return err.Error()
	}
}

// Live is the target as the cluster has it now.
type Live struct {
	Workload *workload.Workload
	Target   digest.TargetIdentity

	// Desired is the template to restore; PreState is the current one.
	// Both stay in memory: the record names their revisions instead.
	Desired     corev1.PodTemplateSpec
	DesiredHash string
	PreState    corev1.PodTemplateSpec

	ManagedBy string
	Replicas  int32

	// from is the revision the rollback replaces, and to the one it restores.
	from, to revisionRef
}

// read reads the target, marking a lost one stale.
func read(ctx context.Context, c client.Reader, t incident.TargetRef) (*workload.Workload, error) {
	w, err := workload.Read(ctx, c, t)
	if workload.Lost(err) {
		return nil, fmt.Errorf("%w: %w", ErrStale, err)
	}
	return w, err
}

// Read reads the target and the template of its revision.
func Read(ctx context.Context, c client.Reader, t incident.TargetRef, revision int64) (Live, error) {
	w, err := read(ctx, c, t)
	if err != nil {
		return Live{}, err
	}
	if w.Revision == revision {
		return Live{}, fmt.Errorf("%w: %d", ErrAlreadyCurrent, revision)
	}
	r, err := w.RevisionAt(ctx, revision)
	if errors.Is(err, workload.ErrNoRevision) {
		return Live{}, fmt.Errorf("%w: %d", ErrNoSuchRevision, revision)
	}
	if err != nil {
		return Live{}, err
	}
	desiredHash, err := TemplateHash(r.Template)
	if err != nil {
		return Live{}, err
	}
	from, err := current(ctx, w)
	if err != nil {
		return Live{}, err
	}
	return Live{
		Workload:    w,
		Target:      w.Identity(),
		Desired:     r.Template,
		DesiredHash: desiredHash,
		PreState:    *w.Template.DeepCopy(),
		ManagedBy:   w.ManagedBy,
		Replicas:    w.Replicas.Desired,
		from:        from,
		to:          refOf(r, desiredHash),
	}, nil
}

// Fields are the digest fields of a rollback of live to revision.
func Fields(incidentID string, live Live, revision int64, policyID, policyDigest string) digest.ActionFields {
	return digest.ActionFields{
		IncidentID:        incidentID,
		Capability:        Name,
		CapabilityVersion: Version,
		Target:            live.Target,
		Params:            Params(revision, live.DesiredHash),
		PolicyID:          policyID,
		PolicyDigest:      policyDigest,
		Constraints:       map[string]any{},
	}
}

// RestoreFields are the digest fields of putting live back to revision, the
// one Themis replaced in action restores: a rollback marked with what it
// undoes.
func RestoreFields(incidentID string, live Live, revision int64, restores, policyID, policyDigest string) digest.ActionFields {
	f := Fields(incidentID, live, revision, policyID, policyDigest)
	f.Params["restores"] = restores
	return f
}

// Params binds the revision and the exact template it restores.
func Params(revision int64, desiredHash string) map[string]any {
	return map[string]any{"revision": revision, "templateHash": desiredHash}
}

// Apply writes Desired with live's resourceVersion as precondition, so a
// write since the read conflicts. It returns the resulting generation.
func Apply(ctx context.Context, c client.Client, live Live, dryRun bool) (int64, error) {
	return live.Workload.SetTemplate(ctx, c, live.Desired, dryRun)
}

// TemplateHash hashes a pod template the way actions bind it.
func TemplateHash(t corev1.PodTemplateSpec) (string, error) { return workload.TemplateHash(t) }
