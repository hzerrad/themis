package evidence

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/incident"
)

// PodStates describes the target's pods: revision, readiness and container
// states.
const PodStates = "pod_states"

// maxPods bounds an item; unhealthy pods are kept first.
const maxPods = 50

const revisionAnnotation = "deployment.kubernetes.io/revision"

// podStatesView describes state only: pod specs often hold literal secrets.
type podStatesView struct {
	Pods      []podView `json:"pods"`
	TotalPods int       `json:"totalPods"`
}

type podView struct {
	Name     string          `json:"name"`
	Revision string          `json:"revision"`
	Phase    corev1.PodPhase `json:"phase"`
	Ready    bool            `json:"ready"`

	StartedAt time.Time `json:"startedAt,omitzero"`

	Containers []containerView `json:"containers"`
}

type containerView struct {
	Name         string `json:"name"`
	Init         bool   `json:"init,omitzero"`
	Image        string `json:"image"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`

	// LastTermination is where a restarted container's OOMKilled shows.
	State           *stateView `json:"state,omitempty"`
	LastTermination *stateView `json:"lastTermination,omitempty"`
}

// stateView leaves messages out: they are whatever the application wrote.
type stateView struct {
	Status     string    `json:"status"` // waiting, running or terminated
	Reason     string    `json:"reason,omitempty"`
	ExitCode   *int32    `json:"exitCode,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

type podStates struct{}

// NewPodStates returns the pod_states template.
func NewPodStates() Template { return podStates{} }

func (podStates) Name() string { return PodStates }

func (podStates) Collect(ctx context.Context, c client.Reader, t incident.TargetRef, _ map[string]string) (Item, error) {
	d, err := getDeployment(ctx, c, t)
	if err != nil {
		return Item{}, err
	}
	replicaSets, err := ownedReplicaSets(ctx, c, d)
	if err != nil {
		return Item{}, err
	}
	revisions := map[types.UID]string{}
	for _, rs := range replicaSets {
		revisions[rs.UID] = rs.Annotations[revisionAnnotation]
	}

	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return Item{}, fmt.Errorf("selector of Deployment %s: %w", t.Name, err)
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(t.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return Item{}, fmt.Errorf("list pods of Deployment %s: %w", t.Name, err)
	}
	var view podStatesView
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil {
			continue
		}
		revision, ok := revisions[owner.UID]
		if !ok {
			continue
		}
		view.Pods = append(view.Pods, viewPod(&pod, revision))
	}

	view.TotalPods = len(view.Pods)
	slices.SortFunc(view.Pods, func(a, b podView) int {
		return cmp.Or(
			cmp.Compare(healthRank(a), healthRank(b)),
			cmp.Compare(a.Name, b.Name),
		)
	})
	truncated := len(view.Pods) > maxPods
	if truncated {
		view.Pods = view.Pods[:maxPods]
	}

	data, err := json.Marshal(view)
	if err != nil {
		return Item{}, err
	}
	return Item{Data: data, Truncated: truncated}, nil
}

func viewPod(pod *corev1.Pod, revision string) podView {
	v := podView{Name: pod.Name, Revision: revision, Phase: pod.Status.Phase}
	if pod.Status.StartTime != nil {
		v.StartedAt = pod.Status.StartTime.UTC()
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			v.Ready = cond.Status == corev1.ConditionTrue
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		v.Containers = append(v.Containers, viewContainer(cs, true))
	}
	for _, cs := range pod.Status.ContainerStatuses {
		v.Containers = append(v.Containers, viewContainer(cs, false))
	}
	return v
}

func viewContainer(cs corev1.ContainerStatus, init bool) containerView {
	return containerView{
		Name:            cs.Name,
		Init:            init,
		Image:           cs.Image,
		Ready:           cs.Ready,
		RestartCount:    cs.RestartCount,
		State:           viewState(cs.State),
		LastTermination: viewState(cs.LastTerminationState),
	}
}

func viewState(s corev1.ContainerState) *stateView {
	switch {
	case s.Waiting != nil:
		return &stateView{Status: "waiting", Reason: s.Waiting.Reason}
	case s.Running != nil:
		return &stateView{Status: "running", StartedAt: s.Running.StartedAt.UTC()}
	case s.Terminated != nil:
		return &stateView{
			Status:     "terminated",
			Reason:     s.Terminated.Reason,
			ExitCode:   &s.Terminated.ExitCode,
			StartedAt:  s.Terminated.StartedAt.UTC(),
			FinishedAt: s.Terminated.FinishedAt.UTC(),
		}
	}
	return nil
}

// healthRank orders pods for truncation: not ready, restarted, healthy.
func healthRank(p podView) int {
	if !p.Ready {
		return 0
	}
	for _, c := range p.Containers {
		if c.RestartCount > 0 {
			return 1
		}
	}
	return 2
}
