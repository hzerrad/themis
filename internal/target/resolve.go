// Package target finds the workload an alert is about: the object its labels
// name, followed up controller owner references to the top.
package target

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"

	"github.com/hzerrad/themis/internal/incident"
)

// Alert labels with a fixed meaning during target resolution.
const (
	LabelNamespace    = "namespace"
	LabelThemisTarget = "themis_target"
)

// LabelKind maps an alert label to the kind of object it names.
type LabelKind struct {
	Label string
	Kind  schema.GroupKind
}

// ObjectLabels are the alert labels that name an object, in the order tried:
// workloads before pod.
var ObjectLabels = []LabelKind{
	{Label: "deployment", Kind: schema.GroupKind{Group: "apps", Kind: "Deployment"}},
	{Label: "statefulset", Kind: schema.GroupKind{Group: "apps", Kind: "StatefulSet"}},
	{Label: "daemonset", Kind: schema.GroupKind{Group: "apps", Kind: "DaemonSet"}},
	{Label: "job_name", Kind: schema.GroupKind{Group: "batch", Kind: "Job"}},
	{Label: "persistentvolumeclaim", Kind: schema.GroupKind{Kind: "PersistentVolumeClaim"}},
	{Label: "pod", Kind: schema.GroupKind{Kind: "Pod"}},
}

// MaxOwnerDepth bounds the owner walk against reference cycles.
const MaxOwnerDepth = 8

// Reasons an alert's target cannot be resolved.
var (
	ErrMissingLabels     = errors.New("alert has no labels naming an object")
	ErrInvalidOverride   = errors.New("themis_target must be <Kind.group>/<name>")
	ErrUnknownKind       = errors.New("kind not served by the cluster")
	ErrNotFound          = errors.New("target not found")
	ErrOwnerChainTooLong = errors.New("owner reference chain too long")
)

// Resolve finds the object an alert is about: themis_target as named, or the
// first ObjectLabels object followed up controller owners to the top.
func Resolve(ctx context.Context, c client.Reader, mapper meta.RESTMapper, labels map[string]string) (incident.TargetRef, error) {
	ns := labels[LabelNamespace]
	if ns == "" {
		return incident.TargetRef{}, ErrMissingLabels
	}

	if override := labels[LabelThemisTarget]; override != "" {
		gk, name, err := parseOverride(override)
		if err != nil {
			return incident.TargetRef{}, err
		}

		obj, gvk, err := lookup(ctx, c, mapper, gk, "", ns, name)
		if err != nil {
			return incident.TargetRef{}, err
		}

		return toTarget(obj, gvk), nil
	}

	for _, entry := range ObjectLabels {
		name := labels[entry.Label]
		if name == "" {
			continue
		}

		obj, gvk, err := lookup(ctx, c, mapper, entry.Kind, "", ns, name)
		if err != nil {
			return incident.TargetRef{}, err
		}

		return walkToTop(ctx, c, mapper, obj, gvk, ns)
	}

	return incident.TargetRef{}, ErrMissingLabels
}

func walkToTop(ctx context.Context, c client.Reader, mapper meta.RESTMapper, obj *metav1.PartialObjectMetadata, gvk schema.GroupVersionKind, ns string) (incident.TargetRef, error) {
	for range MaxOwnerDepth {
		ref := metav1.GetControllerOf(obj)
		if ref == nil {
			return toTarget(obj, gvk), nil
		}

		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return incident.TargetRef{}, fmt.Errorf("owner of %s %s: %w", gvk.Kind, obj.GetName(), err)
		}

		owner := schema.GroupKind{Group: gv.Group, Kind: ref.Kind}
		obj, gvk, err = lookup(ctx, c, mapper, owner, gv.Version, ns, ref.Name)
		if err != nil {
			return incident.TargetRef{}, err
		}

		if obj.GetUID() != ref.UID {
			return incident.TargetRef{}, fmt.Errorf("%w: %s %s was recreated", ErrNotFound, owner, ref.Name)
		}
	}

	return incident.TargetRef{}, ErrOwnerChainTooLong
}

func parseOverride(s string) (schema.GroupKind, string, error) {
	kindGroup, name, found := strings.Cut(s, "/")
	if !found {
		return schema.GroupKind{}, "", fmt.Errorf("%w: %q has no /", ErrInvalidOverride, s)
	}

	if name == "" || strings.Contains(name, "/") {
		return schema.GroupKind{}, "", fmt.Errorf("%w: %q has an invalid name", ErrInvalidOverride, s)
	}

	if !config.IsTargetKind(kindGroup) {
		return schema.GroupKind{}, "", fmt.Errorf("%w: %q is not a Kind.group", ErrInvalidOverride, kindGroup)
	}

	return schema.ParseGroupKind(kindGroup), name, nil
}

func lookup(ctx context.Context, c client.Reader, mapper meta.RESTMapper, gk schema.GroupKind, version, namespace, name string) (*metav1.PartialObjectMetadata, schema.GroupVersionKind, error) {
	var err error
	var m *meta.RESTMapping

	if version != "" {
		m, err = mapper.RESTMapping(gk, version)
	} else {
		m, err = mapper.RESTMapping(gk)
	}
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, schema.GroupVersionKind{}, fmt.Errorf("%w: %s", ErrUnknownKind, gk)
		}

		return nil, schema.GroupVersionKind{}, fmt.Errorf("map %s: %w", gk, err)
	}

	objectKey := client.ObjectKey{Name: name}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		objectKey.Namespace = namespace
	}

	obj := &metav1.PartialObjectMetadata{}
	obj.SetGroupVersionKind(m.GroupVersionKind)
	if err := c.Get(ctx, objectKey, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, schema.GroupVersionKind{}, fmt.Errorf("%w: %s %s", ErrNotFound, gk, name)
		}
		return nil, schema.GroupVersionKind{}, fmt.Errorf("get %s %s: %w", gk, name, err)
	}

	return obj, m.GroupVersionKind, nil
}

func toTarget(obj *metav1.PartialObjectMetadata, gvk schema.GroupVersionKind) incident.TargetRef {
	return incident.TargetRef{
		Group:     gvk.Group,
		Kind:      gvk.Kind,
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		UID:       string(obj.GetUID()),
	}
}
