// Package resources reads Themis's configuration from its custom resources
// and keeps it current without a restart. A resource that fails validation
// is refused in its status, and its previous version keeps applying until
// the core restarts.
package resources

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/telemetry"
)

const (
	Group   = "themis.dev"
	Version = "v1alpha1"

	// ThemisConfigName is the only name a ThemisConfig may have: there is one
	// per cluster.
	ThemisConfigName = "themis"

	conditionAccepted = "Accepted"
)

var (
	ThemisConfigKind    = schema.GroupVersionKind{Group: Group, Version: Version, Kind: "ThemisConfig"}
	IncidentProfileKind = schema.GroupVersionKind{Group: Group, Version: Version, Kind: "IncidentProfile"}
)

// Live is the configuration in force: how Themis is installed, from the file,
// with its resources applied.
type Live struct {
	base config.Config

	// Reader lists resources straight from the API server.
	Reader client.Reader
	// Writer reports on them in their status.
	Writer client.Client
	// Check refuses a profile Themis cannot run, beyond its own validation.
	Check  func(config.Profile) error
	Logger *slog.Logger

	current atomic.Pointer[config.Config]

	mu       sync.Mutex
	settings config.ThemisConfigSpec
	profiles map[string]config.Profile
}

// New returns a Live with no resources read yet.
func New(base config.Config, r client.Reader, w client.Client, check func(config.Profile) error, log *slog.Logger) *Live {
	l := &Live{base: base, Reader: r, Writer: w, Check: check, Logger: log, profiles: map[string]config.Profile{}}
	l.current.Store(&base)
	return l
}

// Current returns the configuration in force.
func (l *Live) Current() config.Config { return *l.current.Load() }

// Refresh reads every resource, applies the valid ones as one change, and
// reports on each in its status.
func (l *Live) Refresh(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	var errs []error
	cfg, reports, err := l.read(ctx)
	if err != nil {
		return err
	}
	l.current.Store(&cfg)
	for _, r := range reports {
		if err := l.report(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// report is what to say in one resource's status.
type report struct {
	object *unstructured.Unstructured
	err    error
	reason string
}

func (l *Live) read(ctx context.Context) (config.Config, []report, error) {
	var reports []report

	settings := l.settings
	tc := &unstructured.Unstructured{}
	tc.SetGroupVersionKind(ThemisConfigKind)
	err := l.Reader.Get(ctx, client.ObjectKey{Name: ThemisConfigName}, tc)
	switch {
	case apierrors.IsNotFound(err):
		settings = config.ThemisConfigSpec{}
	case err != nil:
		return config.Config{}, nil, fmt.Errorf("read ThemisConfig: %w", err)
	default:
		spec, ignored, err := l.parseSettings(tc)
		r := report{object: tc, err: err, reason: "Invalid"}
		if err == nil {
			settings = spec
			r.reason = "Applied"
			if len(ignored) > 0 {
				r.reason = "NotAllEnrolled"
			}
		}
		reports = append(reports, r)
	}
	l.settings = settings

	cfg := l.base
	if len(settings.EnrolledNamespaces) > 0 {
		cfg.EnrolledNamespaces = keepEnrolled(settings.EnrolledNamespaces, l.base.EnrolledNamespaces)
	}

	if settings.ExternalProposals.Window.Duration != 0 || len(settings.ExternalProposals.Sources) > 0 {
		cfg.ExternalProposals = settings.ExternalProposals
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(IncidentProfileKind.GroupVersion().WithKind(IncidentProfileKind.Kind + "List"))
	if err := l.Reader.List(ctx, list); err != nil {
		return config.Config{}, nil, fmt.Errorf("list IncidentProfiles: %w", err)
	}
	// The older resource keeps a contested priority.
	items := list.Items
	slices.SortFunc(items, func(a, b unstructured.Unstructured) int {
		return cmp.Or(a.GetCreationTimestamp().Compare(b.GetCreationTimestamp().Time), cmp.Compare(l.key(&a), l.key(&b)))
	})

	applied := map[string]config.Profile{}
	used := map[string]string{}
	claim := func(p config.Profile) error {
		slot := fmt.Sprintf("%s/%d", p.Scope, p.Priority)
		if other, ok := used[slot]; ok && other != p.Name {
			return fmt.Errorf("priority %d is already used by profile %s", p.Priority, other)
		}
		used[slot] = p.Name
		return nil
	}
	for i := range items {
		item := &items[i]
		k := l.key(item)
		p, err := l.parseProfile(item, cfg)
		reason := "Applied"
		if err == nil {
			err = claim(p)
		}
		if err != nil {
			reason = "Invalid"
			if errors.Is(err, errNotEnrolled) {
				reason = "NotEnrolled"
			} else if prev, ok := l.profiles[k]; ok && claim(prev) == nil {
				applied[k] = prev
			}
		} else {
			applied[k] = p
		}
		reports = append(reports, report{object: item, err: err, reason: reason})
	}
	l.profiles = applied

	cfg.Profiles = nil
	for _, k := range sortedKeys(applied) {
		p := applied[k]
		if settings.AutonomyCeiling == config.ModeObserve {
			p.Policy.Mode = config.ModeObserve
		}
		cfg.Profiles = append(cfg.Profiles, p)
	}
	return cfg, reports, nil
}

var errNotEnrolled = errors.New("the namespace isn't enrolled, so this profile doesn't apply. Ask whoever installs Themis to enroll it")

func (l *Live) parseSettings(u *unstructured.Unstructured) (config.ThemisConfigSpec, []string, error) {
	var spec config.ThemisConfigSpec
	if err := decodeSpec(u, &spec); err != nil {
		return spec, nil, err
	}
	if err := spec.ExternalProposals.Validate(); err != nil {
		return spec, nil, fmt.Errorf("externalProposals: %w", err)
	}
	if m := spec.AutonomyCeiling; m != "" && m != config.ModeGuarded && m != config.ModeObserve {
		return spec, nil, fmt.Errorf("autonomyCeiling must be guarded or observe, got %q", m)
	}
	var ignored []string
	for _, ns := range spec.EnrolledNamespaces {
		if !slices.Contains(l.base.EnrolledNamespaces, ns) {
			ignored = append(ignored, ns)
		}
	}
	if len(spec.EnrolledNamespaces) > 0 && len(ignored) == len(spec.EnrolledNamespaces) {
		return spec, nil, fmt.Errorf("none of enrolledNamespaces was enrolled at install: %s", strings.Join(ignored, ", "))
	}
	return spec, ignored, nil
}

func keepEnrolled(wanted, allowed []string) []string {
	var out []string
	for _, ns := range wanted {
		if slices.Contains(allowed, ns) {
			out = append(out, ns)
		}
	}
	return out
}

func (l *Live) parseProfile(u *unstructured.Unstructured, cfg config.Config) (config.Profile, error) {
	var p config.Profile
	if err := decodeSpec(u, &p); err != nil {
		return p, err
	}
	p.Name = u.GetName()
	if err := p.Validate(); err != nil {
		return p, err
	}
	if l.Check != nil {
		if err := l.Check(p); err != nil {
			return p, err
		}
	}
	if u.GetNamespace() != l.base.Resources.Namespace {
		if !cfg.IsEnrolled(u.GetNamespace()) {
			return p, errNotEnrolled
		}
		p.Name, p.Scope = l.key(u), u.GetNamespace()
	}
	return p, nil
}

// key names a profile the way incidents record it: a platform profile by its
// name, a team's with its namespace, since namespaces can reuse names.
func (l *Live) key(u *unstructured.Unstructured) string {
	if u.GetNamespace() == l.base.Resources.Namespace {
		return u.GetName()
	}
	return u.GetNamespace() + "/" + u.GetName()
}

func decodeSpec(u *unstructured.Unstructured, into any) error {
	spec, _, err := unstructured.NestedFieldNoCopy(u.Object, "spec")
	if err != nil {
		return err
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if err := yaml.UnmarshalStrict(data, into); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	return nil
}

func sortedKeys(m map[string]config.Profile) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// report writes the Accepted condition and the digest of what was read.
func (l *Live) report(ctx context.Context, r report) error {
	status := metav1.ConditionTrue
	message := "Themis is using this configuration."
	if r.reason == "NotAllEnrolled" {
		message = "Themis is using this configuration, except for namespaces not enrolled at install."
	}
	if r.err != nil {
		status, message = metav1.ConditionFalse, r.err.Error()
		if _, kept := l.profiles[l.key(r.object)]; kept {
			message += " The previous version keeps applying."
		}
	}
	conditions := existingConditions(r.object)
	changed := apimeta.SetStatusCondition(&conditions, metav1.Condition{
		Type: conditionAccepted, Status: status, Reason: r.reason, Message: message, ObservedGeneration: r.object.GetGeneration(),
	})
	if !changed {
		return nil
	}
	patch := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": r.object.GetAPIVersion(), "kind": r.object.GetKind(),
		"metadata": map[string]any{"name": r.object.GetName(), "namespace": r.object.GetNamespace()},
	}}
	if err := unstructured.SetNestedField(patch.Object, digestOf(r.object), "status", "digest"); err != nil {
		return err
	}
	out, err := toObjects(conditions)
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedSlice(patch.Object, out, "status", "conditions"); err != nil {
		return err
	}
	if err := l.Writer.Status().Patch(ctx, patch, client.Merge); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("report on %s %s: %w", r.object.GetKind(), r.object.GetName(), err)
	}
	return nil
}

func existingConditions(u *unstructured.Unstructured) []metav1.Condition {
	raw, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []metav1.Condition
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func toObjects(conditions []metav1.Condition) ([]any, error) {
	data, err := json.Marshal(conditions)
	if err != nil {
		return nil, err
	}
	var out []any
	return out, json.Unmarshal(data, &out)
}

func digestOf(u *unstructured.Unstructured) string {
	spec, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec")
	data, _ := json.Marshal(spec) //nolint:errcheck // came from JSON
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// retryAfter retries a failed refresh, beyond the controller's own backoff.
const retryAfter = 30 * time.Second

// SetupWatches refreshes whenever a resource's spec changes or one is removed.
func (l *Live) SetupWatches(mgr ctrl.Manager) error {
	watch := func(gvk schema.GroupVersionKind) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		return u
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("themis-config").
		WithOptions(controller.Options{SkipNameValidation: ptr.To(true), MaxConcurrentReconciles: 1}).
		For(watch(ThemisConfigKind), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(watch(IncidentProfileKind), &handler.EnqueueRequestForObject{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(reconcile.Func(func(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
			if err := l.Refresh(ctx); err != nil {
				l.Logger.WarnContext(ctx, "cannot refresh the configuration", telemetry.FieldError, err)
				return reconcile.Result{RequeueAfter: retryAfter}, nil
			}
			return reconcile.Result{}, nil
		}))
}
