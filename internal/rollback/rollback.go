// Package rollback is the deployment.rollback-to-revision capability: it
// restores the pod template of a prior ReplicaSet, as kubectl rollout undo
// does. Governance and the executor both read live state through it, so they
// bind and check the same identity.
package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
)

const (
	Name    = "deployment.rollback-to-revision"
	Version = "1"
)

var (
	// ErrStale means the target is gone or is another object now.
	ErrStale          = errors.New("target changed")
	ErrNoSuchRevision = errors.New("no ReplicaSet has the revision")
	ErrAlreadyCurrent = errors.New("the revision is already current")
)

const (
	revisionAnnotation = "deployment.kubernetes.io/revision"
	hashLabel          = appsv1.DefaultDeploymentUniqueLabelKey
)

// gitOpsManagers maps field managers and marker prefixes to the GitOps tool.
var gitOpsManagers = map[string]string{
	"argocd-controller":             "argocd",
	"argocd-application-controller": "argocd",
	"kustomize-controller":          "flux",
	"helm-controller":               "flux",
	"argocd.argoproj.io/":           "argocd",
	"kustomize.toolkit.fluxcd.io/":  "flux",
	"helm.toolkit.fluxcd.io/":       "flux",
}

// Live is the target as the cluster has it now.
type Live struct {
	Deployment *appsv1.Deployment
	Target     digest.TargetIdentity

	// Desired is the template to restore; PreState is the current one.
	Desired     corev1.PodTemplateSpec
	DesiredHash string
	PreState    corev1.PodTemplateSpec

	ManagedBy string
	Replicas  int32
}

// TemplateSnapshot identifies the target's current template.
type TemplateSnapshot struct {
	SpecHash string

	// Manager last wrote the template, at ChangedAt.
	Manager   string
	ChangedAt time.Time
}

// Snapshot reads the target's current template identity.
func Snapshot(ctx context.Context, c client.Reader, t incident.TargetRef) (TemplateSnapshot, error) {
	d, err := getTarget(ctx, c, t)
	if err != nil {
		return TemplateSnapshot{}, err
	}
	hash, err := TemplateHash(d.Spec.Template)
	if err != nil {
		return TemplateSnapshot{}, err
	}
	manager, at := templateWriter(d)
	return TemplateSnapshot{SpecHash: hash, Manager: manager, ChangedAt: at}, nil
}

func getTarget(ctx context.Context, c client.Reader, t incident.TargetRef) (*appsv1.Deployment, error) {
	if t.Group != appsv1.GroupName || t.Kind != "Deployment" {
		return nil, fmt.Errorf("%w: %s.%s is not a Deployment", ErrStale, t.Kind, t.Group)
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Name}, &d); apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: Deployment %s is gone", ErrStale, t.Name)
	} else if err != nil {
		return nil, fmt.Errorf("get Deployment %s: %w", t.Name, err)
	}
	if string(d.UID) != t.UID {
		return nil, fmt.Errorf("%w: Deployment %s was recreated", ErrStale, t.Name)
	}
	return &d, nil
}

// Read reads the target and the template of its revision.
func Read(ctx context.Context, c client.Reader, t incident.TargetRef, revision int64) (Live, error) {
	dp, err := getTarget(ctx, c, t)
	if err != nil {
		return Live{}, err
	}
	d := *dp
	if d.Annotations[revisionAnnotation] == strconv.FormatInt(revision, 10) {
		return Live{}, fmt.Errorf("%w: %d", ErrAlreadyCurrent, revision)
	}

	rs, err := replicaSetAt(ctx, c, &d, revision)
	if err != nil {
		return Live{}, err
	}
	desired := *rs.Spec.Template.DeepCopy()
	delete(desired.Labels, hashLabel)

	specHash, err := TemplateHash(d.Spec.Template)
	if err != nil {
		return Live{}, err
	}
	desiredHash, err := TemplateHash(desired)
	if err != nil {
		return Live{}, err
	}
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	return Live{
		Deployment: &d,
		Target: digest.TargetIdentity{
			Group: t.Group, Kind: t.Kind, Namespace: t.Namespace, Name: t.Name, UID: t.UID,
			Generation: d.Generation, SpecHash: specHash, Manager: templateManager(&d),
		},
		Desired:     desired,
		DesiredHash: desiredHash,
		PreState:    *d.Spec.Template.DeepCopy(),
		ManagedBy:   managedBy(&d),
		Replicas:    replicas,
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

// Params binds the revision and the exact template it restores.
func Params(revision int64, desiredHash string) map[string]any {
	return map[string]any{"revision": revision, "templateHash": desiredHash}
}

// Apply writes Desired with live's resourceVersion as precondition, so a
// write since the read conflicts. It returns the resulting generation.
func Apply(ctx context.Context, c client.Client, live Live, dryRun bool) (int64, error) {
	d := live.Deployment.DeepCopy()
	base := live.Deployment.DeepCopy()
	d.Spec.Template = *live.Desired.DeepCopy()
	opts := []client.PatchOption{}
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := c.Patch(ctx, d, patch, opts...); err != nil {
		return 0, fmt.Errorf("patch Deployment %s: %w", d.Name, err)
	}
	return d.Generation, nil
}

func replicaSetAt(ctx context.Context, c client.Reader, d *appsv1.Deployment, revision int64) (*appsv1.ReplicaSet, error) {
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("selector of Deployment %s: %w", d.Name, err)
	}
	var list appsv1.ReplicaSetList
	if err := c.List(ctx, &list, client.InNamespace(d.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list ReplicaSets of Deployment %s: %w", d.Name, err)
	}
	want := strconv.FormatInt(revision, 10)
	for i := range list.Items {
		rs := &list.Items[i]
		if metav1.IsControlledBy(rs, d) && rs.Annotations[revisionAnnotation] == want {
			return rs, nil
		}
	}
	return nil, fmt.Errorf("%w: %d", ErrNoSuchRevision, revision)
}

// TemplateHash hashes a pod template the way actions bind it.
func TemplateHash(t corev1.PodTemplateSpec) (string, error) {
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&t)
	if err != nil {
		return "", fmt.Errorf("convert template: %w", err)
	}
	// Round-trip through JSON so numbers decode the same way on every read.
	b, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return "", err
	}
	return digest.SpecHash(map[string]any{"spec": map[string]any{"template": obj}}, []string{"spec.template"})
}

// templateManager is the field manager that last wrote spec.template.
func templateManager(d *appsv1.Deployment) string {
	manager, _ := templateWriter(d)
	return manager
}

// templateWriter is the field manager that last wrote spec.template, and when.
func templateWriter(d *appsv1.Deployment) (string, time.Time) {
	manager := ""
	var latest *metav1.Time
	for _, mf := range d.ManagedFields {
		if mf.FieldsV1 == nil || !strings.Contains(mf.FieldsV1.GetRawString(), `"f:template"`) {
			continue
		}
		if latest == nil || (mf.Time != nil && latest.Before(mf.Time)) {
			manager, latest = mf.Manager, mf.Time
		}
	}
	if latest == nil {
		return manager, time.Time{}
	}
	return manager, latest.UTC()
}

// managedBy names the GitOps tool that owns d, or is empty.
func managedBy(d *appsv1.Deployment) string {
	for _, kv := range []map[string]string{d.Labels, d.Annotations} {
		for k := range kv {
			for prefix, tool := range gitOpsManagers {
				if strings.HasSuffix(prefix, "/") && strings.HasPrefix(k, prefix) {
					return tool
				}
			}
		}
	}
	for _, mf := range d.ManagedFields {
		if tool, ok := gitOpsManagers[mf.Manager]; ok {
			return tool
		}
	}
	return ""
}
