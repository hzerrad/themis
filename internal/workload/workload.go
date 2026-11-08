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
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// FieldManager is the field manager Themis writes workloads as: a Writer
// of this name is Themis.
const FieldManager = "themis"

// Replicas are the replica counts Kubernetes reports. Observed is whether
// they describe the current generation yet.
type Replicas struct {
	Desired, Total, Updated, Ready, Available int32
	Observed                                  bool
}

// Workload is a target as read once. Its revisions and its pods are each
// read on first use, then kept. Each costs a list call and needs its own
// permission: the executor may list ReplicaSets but not pods.
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
	// Deployer names what else deploys the workload, such as Helm, by the
	// marks it leaves: its next deploy writes the template again, over a
	// change made elsewhere. Empty when nothing shows one.
	Deployer string

	Replicas Replicas

	reader     client.Reader
	deployment *appsv1.Deployment
	revisions  []Revision
	pods       []Pod
	listedRS   bool
	listedPods bool
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
		Writer: writer, WrittenAt: at, ManagedBy: managedBy(&d), Deployer: deployer(&d),
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

// Identity is the workload as an action binds it: a change to any of it
// since an approval makes the approval void.
func (w *Workload) Identity() digest.TargetIdentity {
	t := w.Target
	return digest.TargetIdentity{
		Group: t.Group, Kind: t.Kind, Namespace: t.Namespace, Name: t.Name, UID: t.UID,
		Generation: w.Generation, SpecHash: w.TemplateHash, Manager: w.Writer,
	}
}

// AtOnce is how many pods a rollout of a new pod template replaces at a
// time: all of them for the Recreate strategy; for a rolling update, the
// pods it may add above the desired count and take below it, as Kubernetes
// rounds them, and at least one.
func (w *Workload) AtOnce() int32 {
	n := w.Replicas.Desired
	s := w.deployment.Spec.Strategy
	if s.Type == appsv1.RecreateDeploymentStrategyType {
		return n
	}
	quarter := intstr.FromString("25%")
	surge, unavailable := &quarter, &quarter
	if r := s.RollingUpdate; r != nil {
		if r.MaxSurge != nil {
			surge = r.MaxSurge
		}
		if r.MaxUnavailable != nil {
			unavailable = r.MaxUnavailable
		}
	}
	up, err := intstr.GetScaledValueFromIntOrPercent(surge, int(n), true)
	if err != nil {
		up = 1
	}
	down, err := intstr.GetScaledValueFromIntOrPercent(unavailable, int(n), false)
	if err != nil {
		down = 0
	}
	return min(int32(max(up+down, 1)), n) //nolint:gosec // at most n, an int32
}

// SetTemplate writes template as the workload's pod template, with the
// resourceVersion it was read at as precondition, so a write since conflicts.
// dryRun has the API server check the write without making it. It returns
// the resulting generation.
func (w *Workload) SetTemplate(ctx context.Context, c client.Client, template corev1.PodTemplateSpec, dryRun bool) (int64, error) {
	d := w.deployment.DeepCopy()
	d.Spec.Template = *template.DeepCopy()
	opts := []client.PatchOption{}
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	patch := client.MergeFromWithOptions(w.deployment, client.MergeFromWithOptimisticLock{})
	if err := c.Patch(ctx, d, patch, opts...); err != nil {
		return 0, fmt.Errorf("patch Deployment %s: %w", d.Name, err)
	}
	return d.Generation, nil
}

// Revisions are the revisions Kubernetes keeps, in no particular order.
func (w *Workload) Revisions(ctx context.Context) ([]Revision, error) {
	if w.listedRS {
		return w.revisions, nil
	}
	sets, err := list[*appsv1.ReplicaSetList](ctx, w, &appsv1.ReplicaSetList{}, "ReplicaSets")
	if err != nil {
		return nil, err
	}
	var revisions []Revision
	for i := range sets.Items {
		rs := &sets.Items[i]
		if !metav1.IsControlledBy(rs, w.deployment) {
			continue
		}
		template := *rs.Spec.Template.DeepCopy()
		delete(template.Labels, hashLabel)
		revisions = append(revisions, Revision{Number: revisionOf(rs), Template: template, Object: rs})
	}
	w.revisions, w.listedRS = revisions, true
	return revisions, nil
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
	if w.listedPods {
		return w.pods, nil
	}
	revisions, err := w.Revisions(ctx)
	if err != nil {
		return nil, err
	}
	numbers := map[types.UID]int64{}
	for _, r := range revisions {
		numbers[r.Object.UID] = r.Number
	}
	podList, err := list[*corev1.PodList](ctx, w, &corev1.PodList{}, "pods")
	if err != nil {
		return nil, err
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
	w.pods, w.listedPods = pods, true
	return pods, nil
}

// list lists objects of the kind of into that the Deployment's selector
// matches. Callers keep only what the Deployment controls: selectors of
// workloads can overlap.
func list[L client.ObjectList](ctx context.Context, w *Workload, into L, what string) (L, error) {
	d := w.deployment
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return into, fmt.Errorf("selector of Deployment %s: %w", d.Name, err)
	}
	if err := w.reader.List(ctx, into, client.InNamespace(d.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return into, fmt.Errorf("list %s of Deployment %s: %w", what, d.Name, err)
	}
	return into, nil
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

// deployerMarks are the labels and annotations, by key or by prefix, that
// tools other than GitOps write on the workloads they deploy, and the tool
// each names. app.kubernetes.io/managed-by names its tool itself.
var deployerMarks = map[string]string{
	"meta.helm.sh/release-name": "Helm",
	"helm.sh/chart":             "Helm",
	"app.gitlab.com/":           "GitLab",
	"moniker.spinnaker.io/":     "Spinnaker",
	"artifact.spinnaker.io/":    "Spinnaker",
	"skaffold.dev/":             "Skaffold",
	"kapp.k14s.io/":             "kapp",
	"pulumi.com/":               "Pulumi",
}

// deployerManagers are the field managers of tools that deploy workloads
// from a manifest of their own.
var deployerManagers = map[string]string{
	"helm":              "Helm",
	"Terraform":         "Terraform",
	"terraform":         "Terraform",
	"pulumi-kubernetes": "Pulumi",
	"kapp":              "kapp",
	"skaffold":          "Skaffold",
}

const managedByLabel = "app.kubernetes.io/managed-by"

// deployer names the tool, other than GitOps, that deploys d, or is empty.
func deployer(d *appsv1.Deployment) string {
	if v := d.Labels[managedByLabel]; v != "" {
		return v
	}
	for _, kv := range []map[string]string{d.Labels, d.Annotations} {
		for k := range kv {
			for mark, tool := range deployerMarks {
				if k == mark || strings.HasSuffix(mark, "/") && strings.HasPrefix(k, mark) {
					return tool
				}
			}
		}
	}
	for _, mf := range d.ManagedFields {
		if tool, ok := deployerManagers[mf.Manager]; ok {
			return tool
		}
	}
	return ""
}

// Digest is the image digest the workload's pods run for container when its
// image is image, such as sha256:4b5c…, read from a pod that runs it. Empty
// when no pod does, as when the revision's pods are gone, or none pulled it.
func (w *Workload) Digest(ctx context.Context, container, image string) (string, error) {
	pods, err := w.Pods(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range pods {
		specs := slices.Concat(p.Spec.InitContainers, p.Spec.Containers)
		if !slices.ContainsFunc(specs, func(c corev1.Container) bool { return c.Name == container && c.Image == image }) {
			continue
		}
		for _, cs := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
			if i := strings.Index(cs.ImageID, "sha256:"); cs.Name == container && i >= 0 {
				return cs.ImageID[i:], nil
			}
		}
	}
	return "", nil
}

// State is how the workload's current revision is doing: what Kubernetes
// reports about its rollout, and the trouble its containers had. It reports;
// what counts as healthy is the reader's call.
type State struct {
	Desired, Ready int32

	// Updated is set once every pod runs the current template, healthy or
	// not; RolloutComplete once they are all available too.
	Updated, RolloutComplete bool

	// Restarts counts container restarts in the current revision's pods,
	// and OOMKills those since a given time that ran out of memory.
	Restarts int32
	OOMKills int
}

// State reads how the current revision is doing, counting OOM kills since.
func (w *Workload) State(ctx context.Context, since time.Time) (State, error) {
	r := w.Replicas
	s := State{
		Desired: r.Desired, Ready: r.Ready,
		Updated:         r.Observed && r.Updated == r.Desired,
		RolloutComplete: r.Observed && r.Updated == r.Desired && r.Available == r.Desired && r.Total == r.Desired,
	}
	pods, err := w.Pods(ctx)
	if err != nil {
		return State{}, err
	}
	for _, p := range pods {
		if p.Revision != w.Revision {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			s.Restarts += cs.RestartCount
			if term := cs.LastTerminationState.Terminated; term != nil && term.Reason == "OOMKilled" && !term.FinishedAt.Time.Before(since) {
				s.OOMKills++
			}
		}
	}
	return s, nil
}
