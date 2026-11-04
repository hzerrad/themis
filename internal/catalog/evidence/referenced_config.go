package evidence

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/workload"
)

// ReferencedConfig lists the ConfigMaps and Secrets the target's pod template
// refers to, and when each ConfigMap last changed. It never reads their
// values, and never reads a Secret at all.
const ReferencedConfig = "referenced_config"

// Kinds of referenced configuration.
const (
	ConfigKindConfigMap = "ConfigMap"
	ConfigKindSecret    = "Secret"
)

type ReferencedConfigView struct {
	Refs []ConfigRefView `json:"refs"`
}

type ConfigRefView struct {
	Kind string `json:"kind"`
	Name string `json:"name"`

	// Optional means pods start without it.
	Optional bool `json:"optional,omitzero"`

	// Missing is set for a ConfigMap that does not exist.
	Missing bool `json:"missing,omitzero"`

	// ChangedAt is when a ConfigMap's data was last written. It is left out
	// when the object does not say, and for Secrets, which Themis does not read.
	ChangedAt time.Time `json:"changedAt,omitzero"`
}

type referencedConfig struct{}

// NewReferencedConfig returns the referenced_config template.
func NewReferencedConfig() Template { return referencedConfig{} }

func (referencedConfig) Name() string { return ReferencedConfig }

func (referencedConfig) Description() string {
	return "The ConfigMaps and Secrets the pod template refers to, and when each ConfigMap last changed. Names and times only, never values."
}

func (referencedConfig) Params() []string { return nil }

func (referencedConfig) Collect(ctx context.Context, c client.Reader, w *workload.Workload, _ map[string]string) (Item, error) {
	view := ReferencedConfigView{Refs: configRefs(w.Template.Spec)}
	for i, r := range view.Refs {
		if r.Kind != ConfigKindConfigMap {
			continue
		}
		var cm corev1.ConfigMap
		err := c.Get(ctx, client.ObjectKey{Namespace: w.Target.Namespace, Name: r.Name}, &cm)
		switch {
		case apierrors.IsNotFound(err):
			view.Refs[i].Missing = true
		case err != nil:
			return Item{}, fmt.Errorf("get ConfigMap %s: %w", r.Name, err)
		default:
			view.Refs[i].ChangedAt = lastWrite(&cm)
		}
	}
	data, err := json.Marshal(view)
	if err != nil {
		return Item{}, err
	}
	return Item{Data: data}, nil
}

// lastWrite is the newest time a field manager wrote data or binaryData, or
// zero when none is kept. Managers that own only metadata do not count.
func lastWrite(cm *corev1.ConfigMap) time.Time {
	var latest time.Time
	for _, mf := range cm.ManagedFields {
		if mf.Time != nil && mf.Time.After(latest) && writesContent(mf) {
			latest = mf.Time.UTC()
		}
	}
	return latest
}

// writesContent reports whether the entry owns data or binaryData. An entry
// whose fields can't be read counts as a content write, to stay safe.
func writesContent(mf metav1.ManagedFieldsEntry) bool {
	if mf.FieldsType != "FieldsV1" || mf.FieldsV1 == nil {
		return true
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(mf.FieldsV1.GetRawBytes(), &fields); err != nil {
		return true
	}
	_, data := fields["f:data"]
	_, binary := fields["f:binaryData"]
	return data || binary
}

// configRefs lists each reference once. A reference is optional only if every
// use of it is.
func configRefs(spec corev1.PodSpec) []ConfigRefView {
	seen := map[[2]string]bool{}
	add := func(kind, name string, optional *bool) {
		if name == "" {
			return
		}
		key := [2]string{kind, name}
		isOptional := optional != nil && *optional
		if was, ok := seen[key]; ok {
			isOptional = isOptional && was
		}
		seen[key] = isOptional
	}
	for _, v := range spec.Volumes {
		if s := v.ConfigMap; s != nil {
			add(ConfigKindConfigMap, s.Name, s.Optional)
		}
		if s := v.Secret; s != nil {
			add(ConfigKindSecret, s.SecretName, s.Optional)
		}
		if v.Projected == nil {
			continue
		}
		for _, p := range v.Projected.Sources {
			if s := p.ConfigMap; s != nil {
				add(ConfigKindConfigMap, s.Name, s.Optional)
			}
			if s := p.Secret; s != nil {
				add(ConfigKindSecret, s.Name, s.Optional)
			}
		}
	}
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		for _, from := range c.EnvFrom {
			if s := from.ConfigMapRef; s != nil {
				add(ConfigKindConfigMap, s.Name, s.Optional)
			}
			if s := from.SecretRef; s != nil {
				add(ConfigKindSecret, s.Name, s.Optional)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if s := e.ValueFrom.ConfigMapKeyRef; s != nil {
				add(ConfigKindConfigMap, s.Name, s.Optional)
			}
			if s := e.ValueFrom.SecretKeyRef; s != nil {
				add(ConfigKindSecret, s.Name, s.Optional)
			}
		}
	}
	refs := make([]ConfigRefView, 0, len(seen))
	for key, optional := range seen {
		refs = append(refs, ConfigRefView{Kind: key[0], Name: key[1], Optional: optional})
	}
	slices.SortFunc(refs, func(a, b ConfigRefView) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})
	return refs
}
