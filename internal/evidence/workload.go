package evidence

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/incident"
)

// getDeployment reads a Deployment target, checking it is the same object.
func getDeployment(ctx context.Context, c client.Reader, t incident.TargetRef) (*appsv1.Deployment, error) {
	if t.Group != appsv1.GroupName || t.Kind != "Deployment" {
		return nil, fmt.Errorf("%w: %s.%s", ErrUnsupportedTarget, t.Kind, t.Group)
	}

	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Name}, &d); apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: Deployment %s", ErrTargetGone, t.Name)
	} else if err != nil {
		return nil, fmt.Errorf("get Deployment %s: %w", t.Name, err)
	}
	if string(d.UID) != t.UID {
		return nil, fmt.Errorf("%w: Deployment %s was recreated", ErrTargetGone, t.Name)
	}
	return &d, nil
}

// ownedReplicaSets lists the ReplicaSets d controls. Ownership decides, not
// labels: selectors of workloads can overlap.
func ownedReplicaSets(ctx context.Context, c client.Reader, d *appsv1.Deployment) ([]*appsv1.ReplicaSet, error) {
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("selector of Deployment %s: %w", d.Name, err)
	}
	var list appsv1.ReplicaSetList
	if err := c.List(ctx, &list, client.InNamespace(d.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list ReplicaSets of Deployment %s: %w", d.Name, err)
	}
	var owned []*appsv1.ReplicaSet
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], d) {
			owned = append(owned, &list.Items[i])
		}
	}
	return owned, nil
}

// ownedPods lists the pods controlled by replicaSets.
func ownedPods(ctx context.Context, c client.Reader, d *appsv1.Deployment, replicaSets []*appsv1.ReplicaSet) ([]*corev1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("selector of Deployment %s: %w", d.Name, err)
	}
	var list corev1.PodList
	if err := c.List(ctx, &list, client.InNamespace(d.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list pods of Deployment %s: %w", d.Name, err)
	}
	owners := map[types.UID]bool{}
	for _, rs := range replicaSets {
		owners[rs.UID] = true
	}
	var owned []*corev1.Pod
	for i := range list.Items {
		if ref := metav1.GetControllerOf(&list.Items[i]); ref != nil && owners[ref.UID] {
			owned = append(owned, &list.Items[i])
		}
	}
	return owned, nil
}
