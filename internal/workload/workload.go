// Package workload reads the object an incident targets, as Themis sees it:
// its identity, its pod template, its revisions, the pods it runs and their
// state. Every other package asks here, so "the same workload" and "its
// current pods" mean one thing everywhere. Deployments are the only kind
// today.
package workload

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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
)

// Why a target can't be read as the workload an incident is about. An
// *Error carries which one, and to what.
var (
	ErrGone        = errors.New("was deleted")
	ErrReplaced    = errors.New("was replaced by a new object with the same name")
	ErrUnsupported = errors.New("is not a kind Themis can act on")

	// ErrNoRevision means Kubernetes no longer keeps the revision asked for.
	ErrNoRevision = errors.New("no such revision")
)

// Error says which of ErrGone, ErrReplaced and ErrUnsupported happened to
// Target. Found is the UID of the object now under Target's name, when it
// was replaced.
type Error struct {
	Case   error
	Target incident.TargetRef
	Found  string
}

func (e *Error) Error() string {
	t := e.Target
	msg := fmt.Sprintf("%s %s in %s %s", t.Kind, t.Name, t.Namespace, e.Case)
	if e.Found != "" {
		msg += fmt.Sprintf(": expected uid %s, found %s", t.UID, e.Found)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Case }

// Lost reports whether err says the target itself is gone, replaced or of an
// unsupported kind, as opposed to failing to read it.
func Lost(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

const (
	revisionAnnotation = "deployment.kubernetes.io/revision"
	hashLabel          = appsv1.DefaultDeploymentUniqueLabelKey
)

// Replicas are the replica counts Kubernetes reports. Observed is whether
// they describe the current generation yet.
type Replicas struct {
	Desired, Total, Updated, Ready, Available int32
	Observed                                  bool
}

// Workload is a target as read once. Its revisions and pods are read on
// first use, then kept, so they agree with each other.
type Workload struct {
	Target     incident.TargetRef
	Generation int64

	// Revision is the current revision, 0 if Kubernetes hasn't numbered it.
	Revision int64

	// Template is the current pod template; TemplateHash binds it.
	Template     corev1.PodTemplateSpec
	TemplateHash string

	// Writer is the field manager that last wrote the template, at WrittenAt.
	Writer    string
	WrittenAt time.Time

	// ManagedBy names the GitOps tool that owns the workload, or is empty.
	ManagedBy string

	Replicas Replicas

	reader     client.Reader
	deployment *appsv1.Deployment
	revisions  []Revision
	pods       []Pod
	listed     bool
}

// Revision is one revision Kubernetes keeps of a workload.
type Revision struct {
	// Number is 0 if Kubernetes hasn't numbered it.
	Number int64

	// Template is the revision's pod template, without Kubernetes' own
	// per-revision label, as a rollback would write it.
	Template corev1.PodTemplateSpec

	// Object is the ReplicaSet that holds it.
	Object *appsv1.ReplicaSet
}

// Pod is one pod of a workload, with the revision it runs.
type Pod struct {
	*corev1.Pod
	Revision int64
}

// Read reads t, checking it is still the object the incident is about.
func Read(ctx context.Context, c client.Reader, t incident.TargetRef) (*Workload, error) {
	if t.Group != appsv1.GroupName || t.Kind != "Deployment" {
		return nil, &Error{Case: ErrUnsupported, Target: t}
	}
	var d appsv1.Deployment
	err := c.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Name}, &d)
	switch {
	case apierrors.IsNotFound(err):
		return nil, &Error{Case: ErrGone, Target: t}
	case err != nil:
		return nil, fmt.Errorf("get Deployment %s in %s: %w", t.Name, t.Namespace, err)
	case string(d.UID) != t.UID:
		return nil, &Error{Case: ErrReplaced, Target: t, Found: string(d.UID)}
	}
	hash, err := TemplateHash(d.Spec.Template)
	if err != nil {
		return nil, err
	}
	writer, at := templateWriter(&d)
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	s := d.Status
	return &Workload{
		Target: t, Generation: d.Generation, Revision: revisionOf(&d),
		Template: *d.Spec.Template.DeepCopy(), TemplateHash: hash,
		Writer: writer, WrittenAt: at, ManagedBy: managedBy(&d),
		Replicas: Replicas{
			Desired: desired, Total: s.Replicas, Updated: s.UpdatedReplicas, Ready: s.ReadyReplicas,
			Available: s.AvailableReplicas, Observed: s.ObservedGeneration >= d.Generation,
		},
		reader: c, deployment: &d,
	}, nil
}

// Object is the Deployment as read, for the code that writes it back with
// its resourceVersion as precondition, or renders fields only it has.
func (w *Workload) Object() *appsv1.Deployment { return w.deployment }

// Revisions are the revisions Kubernetes keeps, in no particular order.
func (w *Workload) Revisions(ctx context.Context) ([]Revision, error) {
	if err := w.list(ctx); err != nil {
		return nil, err
	}
	return w.revisions, nil
}

// RevisionAt returns revision n.
func (w *Workload) RevisionAt(ctx context.Context, n int64) (Revision, error) {
	revisions, err := w.Revisions(ctx)
	if err != nil {
		return Revision{}, err
	}
	for _, r := range revisions {
		if r.Number == n {
			return r, nil
		}
	}
	return Revision{}, fmt.Errorf("%w: %d", ErrNoRevision, n)
}

// Pods are the workload's pods, of every revision it keeps.
func (w *Workload) Pods(ctx context.Context) ([]Pod, error) {
	if err := w.list(ctx); err != nil {
		return nil, err
	}
	return w.pods, nil
}

// list reads the ReplicaSets and pods the Deployment controls. Ownership
// decides, not labels: selectors of workloads can overlap.
func (w *Workload) list(ctx context.Context) error {
	if w.listed {
		return nil
	}
	d := w.deployment
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return fmt.Errorf("selector of Deployment %s: %w", d.Name, err)
	}
	in := []client.ListOption{client.InNamespace(d.Namespace), client.MatchingLabelsSelector{Selector: selector}}
	var sets appsv1.ReplicaSetList
	if err := w.reader.List(ctx, &sets, in...); err != nil {
		return fmt.Errorf("list ReplicaSets of Deployment %s: %w", d.Name, err)
	}
	numbers := map[types.UID]int64{}
	var revisions []Revision
	for i := range sets.Items {
		rs := &sets.Items[i]
		if !metav1.IsControlledBy(rs, d) {
			continue
		}
		n := revisionOf(rs)
		numbers[rs.UID] = n
		template := *rs.Spec.Template.DeepCopy()
		delete(template.Labels, hashLabel)
		revisions = append(revisions, Revision{Number: n, Template: template, Object: rs})
	}
	var podList corev1.PodList
	if err := w.reader.List(ctx, &podList, in...); err != nil {
		return fmt.Errorf("list pods of Deployment %s: %w", d.Name, err)
	}
	var pods []Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if ref := metav1.GetControllerOf(p); ref != nil {
			if n, ok := numbers[ref.UID]; ok {
				pods = append(pods, Pod{Pod: p, Revision: n})
			}
		}
	}
	w.revisions, w.pods, w.listed = revisions, pods, true
	return nil
}

func revisionOf(obj metav1.Object) int64 {
	n, err := strconv.ParseInt(obj.GetAnnotations()[revisionAnnotation], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// TemplateHash hashes a pod template the way actions bind it. Approvals,
// actions and incidents store it, so its output must never change.
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
