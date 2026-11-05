package evidence

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/workload"
)

// RolloutHistory describes the target's rollout: its progress, ownership, and
// what each revision runs.
const RolloutHistory = "rollout_history"

// maxRevisions bounds an item; the newest revisions are kept.
const maxRevisions = 20

const revisionHistoryAnnotation = "deployment.kubernetes.io/revision-history"

// gitOpsMarkers are the label and annotation prefixes that mark an object as
// managed by a GitOps controller.
var gitOpsMarkers = []string{
	"argocd.argoproj.io/",
	"kustomize.toolkit.fluxcd.io/",
	"helm.toolkit.fluxcd.io/",
}

type RolloutHistoryView struct {
	Deployment     DeploymentView `json:"deployment"`
	Revisions      []RevisionView `json:"revisions"`
	TotalRevisions int            `json:"totalRevisions"`
}

type DeploymentView struct {
	Name                string          `json:"name"`
	Revision            string          `json:"currentRevision"`
	Generation          int64           `json:"generation"`
	ObservedGeneration  int64           `json:"observedGeneration"`
	Replicas            int32           `json:"replicas"`
	UpdatedReplicas     int32           `json:"updatedReplicas"`
	ReadyReplicas       int32           `json:"readyReplicas"`
	UnavailableReplicas int32           `json:"unavailableReplicas"`
	Paused              bool            `json:"paused,omitzero"`
	Conditions          []ConditionView `json:"conditions,omitempty"`

	// TemplateChangedAt is when the pod template was last written: a rollout
	// that reuses an old ReplicaSet starts then, not at its creation.
	TemplateChangedAt time.Time `json:"templateChangedAt,omitzero"`

	// Markers are GitOps labels and annotations; Managers are field managers.
	Markers  map[string]string `json:"ownershipMarkers,omitempty"`
	Managers []string          `json:"fieldManagers,omitempty"`
}

type ConditionView struct {
	Type           string    `json:"type"`
	Status         string    `json:"status"`
	Reason         string    `json:"reason,omitempty"`
	Message        string    `json:"message,omitempty"`
	LastUpdate     time.Time `json:"lastUpdateTime,omitzero"`
	LastTransition time.Time `json:"lastTransitionTime,omitzero"`
}

type RevisionView struct {
	ReplicaSet string `json:"replicaSet"`
	Revision   string `json:"revision"`

	// FormerRevisions are the numbers this ReplicaSet had before rollbacks.
	FormerRevisions string    `json:"formerRevisions,omitempty"`
	Current         bool      `json:"current,omitzero"`
	CreatedAt       time.Time `json:"createdAt,omitzero"`
	Replicas        int32     `json:"replicas"`
	ReadyReplicas   int32     `json:"readyReplicas"`

	Containers []TemplateContainerView `json:"containers"`
}

// TemplateContainerView leaves out env, args and commands, which hold secrets.
type TemplateContainerView struct {
	Name     string            `json:"name"`
	Image    string            `json:"image"`
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`

	// Probes are the timing of each probe the container has, by kind:
	// liveness, readiness or startup.
	Probes map[string]ProbeView `json:"probes,omitempty"`
}

// ProbeView is a probe's timing, with Kubernetes's default for a field left
// unset. What the probe calls is left out: its headers and commands can hold
// secrets.
type ProbeView struct {
	InitialDelaySeconds int32 `json:"initialDelaySeconds"`
	PeriodSeconds       int32 `json:"periodSeconds"`
	TimeoutSeconds      int32 `json:"timeoutSeconds"`
	FailureThreshold    int32 `json:"failureThreshold"`
	SuccessThreshold    int32 `json:"successThreshold"`
}

type rolloutHistory struct{}

// NewRolloutHistory returns the rollout_history template.
func NewRolloutHistory() Template { return rolloutHistory{} }

func (rolloutHistory) Name() string { return RolloutHistory }

func (rolloutHistory) Description() string {
	return "The Deployment's rollout progress, conditions, GitOps ownership markers, and each ReplicaSet revision with its images and resource limits."
}

func (rolloutHistory) Params() []string { return nil }

func (rolloutHistory) Collect(ctx context.Context, _ client.Reader, w *workload.Workload, _ map[string]string) (Item, error) {
	revisions, err := w.Revisions(ctx)
	if err != nil {
		return Item{}, err
	}

	view := RolloutHistoryView{Deployment: viewDeployment(w.Object())}
	for _, r := range revisions {
		view.Revisions = append(view.Revisions, viewRevision(r.Object, view.Deployment.Revision))
	}
	view.TotalRevisions = len(view.Revisions)
	slices.SortFunc(view.Revisions, func(a, b RevisionView) int {
		return cmp.Or(
			cmp.Compare(revisionNumber(b.Revision), revisionNumber(a.Revision)),
			cmp.Compare(a.ReplicaSet, b.ReplicaSet),
		)
	})
	truncated := len(view.Revisions) > maxRevisions
	if truncated {
		view.Revisions = view.Revisions[:maxRevisions]
	}

	data, err := json.Marshal(view, json.Deterministic(true))
	if err != nil {
		return Item{}, err
	}
	return Item{Data: data, Truncated: truncated}, nil
}

// revisionNumber is -1 for a missing or malformed revision, so it sorts last.
func revisionNumber(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func viewDeployment(d *appsv1.Deployment) DeploymentView {
	v := DeploymentView{
		Name:                d.Name,
		Revision:            d.Annotations[revisionAnnotation],
		Generation:          d.Generation,
		ObservedGeneration:  d.Status.ObservedGeneration,
		Replicas:            d.Status.Replicas,
		UpdatedReplicas:     d.Status.UpdatedReplicas,
		ReadyReplicas:       d.Status.ReadyReplicas,
		UnavailableReplicas: d.Status.UnavailableReplicas,
		Paused:              d.Spec.Paused,
		Markers:             gitOpsMarkersOf(d),
	}
	for _, cond := range d.Status.Conditions {
		v.Conditions = append(v.Conditions, ConditionView{
			Type:           string(cond.Type),
			Status:         string(cond.Status),
			Reason:         cond.Reason,
			Message:        bounded(cond.Message, maxMessageBytes),
			LastUpdate:     cond.LastUpdateTime.UTC(),
			LastTransition: cond.LastTransitionTime.UTC(),
		})
	}
	for _, mf := range d.ManagedFields {
		if !slices.Contains(v.Managers, mf.Manager) {
			v.Managers = append(v.Managers, mf.Manager)
		}
		if mf.Time != nil && mf.FieldsV1 != nil && strings.Contains(mf.FieldsV1.GetRawString(), `"f:template"`) && mf.Time.After(v.TemplateChangedAt) {
			v.TemplateChangedAt = mf.Time.UTC()
		}
	}
	return v
}

func gitOpsMarkersOf(obj metav1.Object) map[string]string {
	markers := map[string]string{}
	for _, kv := range []map[string]string{obj.GetLabels(), obj.GetAnnotations()} {
		for k, v := range kv {
			for _, prefix := range gitOpsMarkers {
				if strings.HasPrefix(k, prefix) {
					markers[k] = v
				}
			}
		}
	}
	if len(markers) == 0 {
		return nil
	}
	return markers
}

func viewRevision(rs *appsv1.ReplicaSet, current string) RevisionView {
	v := RevisionView{
		ReplicaSet:      rs.Name,
		Revision:        rs.Annotations[revisionAnnotation],
		FormerRevisions: rs.Annotations[revisionHistoryAnnotation],
		CreatedAt:       rs.CreationTimestamp.UTC(),
		Replicas:        rs.Status.Replicas,
		ReadyReplicas:   rs.Status.ReadyReplicas,
	}
	v.Current = v.Revision != "" && v.Revision == current
	for _, ctr := range rs.Spec.Template.Spec.Containers {
		v.Containers = append(v.Containers, TemplateContainerView{
			Name:     ctr.Name,
			Image:    ctr.Image,
			Requests: quantities(ctr.Resources.Requests),
			Limits:   quantities(ctr.Resources.Limits),
			Probes:   probes(&ctr),
		})
	}
	return v
}

func probes(c *corev1.Container) map[string]ProbeView {
	m := map[string]ProbeView{}
	for kind, p := range map[string]*corev1.Probe{"liveness": c.LivenessProbe, "readiness": c.ReadinessProbe, "startup": c.StartupProbe} {
		if p != nil {
			m[kind] = ProbeView{
				InitialDelaySeconds: p.InitialDelaySeconds,
				PeriodSeconds:       cmp.Or(p.PeriodSeconds, 10),
				TimeoutSeconds:      cmp.Or(p.TimeoutSeconds, 1),
				FailureThreshold:    cmp.Or(p.FailureThreshold, 3),
				SuccessThreshold:    cmp.Or(p.SuccessThreshold, 1),
			}
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func quantities(l corev1.ResourceList) map[string]string {
	if len(l) == 0 {
		return nil
	}
	m := make(map[string]string, len(l))
	for name, q := range l {
		m[string(name)] = q.String()
	}
	return m
}
