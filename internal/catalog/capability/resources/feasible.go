package resources

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/capability"
)

// Feasible checks the cluster can run the planned template: the namespace's
// LimitRanges allow its values, a node can fit a pod that asks for more, and
// the namespace's ResourceQuotas leave room to roll it out. Each refusal says
// which constraint, and by how much.
func (Capability) Feasible(ctx context.Context, r client.Reader, p capability.Plan) error {
	s, ok := p.State.(planned)
	if !ok {
		return errors.New("not a set-resources plan")
	}
	ns := s.workload.Target.Namespace
	var ranges corev1.LimitRangeList
	if err := list(ctx, r, &ranges, "LimitRanges", client.InNamespace(ns)); err != nil {
		return err
	}
	if err := withinLimitRanges(ranges.Items, s); err != nil {
		return err
	}
	before, after := podTotals(s.workload.Template, ranges.Items), podTotals(s.after, ranges.Items)

	var nodes corev1.NodeList
	if err := list(ctx, r, &nodes, "nodes"); err != nil {
		return err
	}
	if err := fits(nodes.Items, s.after.Spec, before, after); err != nil {
		return err
	}

	var quotas corev1.ResourceQuotaList
	if err := list(ctx, r, &quotas, "ResourceQuotas", client.InNamespace(ns)); err != nil {
		return err
	}
	replicas := s.workload.Replicas.Desired
	return withinQuotas(quotas.Items, before, after, int64(replicas), surge(s, replicas))
}

// list lists what into holds. Not being allowed to is a refusal: Themis
// can't tell whether the change would work.
func list(ctx context.Context, r client.Reader, into client.ObjectList, what string, opts ...client.ListOption) error {
	err := r.List(ctx, into, opts...)
	if apierrors.IsForbidden(err) {
		return &capability.Refusal{Reason: fmt.Sprintf("Themis can't tell whether the cluster can run this change: it isn't allowed to read %s.", what)}
	}
	if err != nil {
		return fmt.Errorf("list %s: %w", what, err)
	}
	return nil
}

// totals are a pod's requests and limits, in thousandths of a unit, as
// Kubernetes counts them for scheduling and quotas.
type totals map[field]int64

// podTotals adds up what a pod of t asks for, after Kubernetes' defaults and
// the LimitRanges': its containers together, or its largest init container
// if that asks for more.
func podTotals(t corev1.PodTemplateSpec, ranges []corev1.LimitRange) totals {
	out, init := totals{}, totals{}
	for _, c := range t.Spec.Containers {
		for f, q := range effective(c, ranges) {
			out[f] += q.MilliValue()
		}
	}
	for _, c := range t.Spec.InitContainers {
		for f, q := range effective(c, ranges) {
			init[f] = max(init[f], q.MilliValue())
		}
	}
	for f, v := range init {
		out[f] = max(out[f], v)
	}
	return out
}

// effective is what c asks for once admitted: a request it doesn't set is
// its limit, then the LimitRanges' defaults fill what is still unset.
func effective(c corev1.Container, ranges []corev1.LimitRange) map[field]resource.Quantity {
	out := map[field]resource.Quantity{}
	for _, f := range fields {
		if q := f.get(&c); q != nil {
			out[f] = *q
		}
	}
	for _, res := range []corev1.ResourceName{corev1.ResourceMemory, corev1.ResourceCPU} {
		req, lim := field{"requests", res}, field{"limits", res}
		if _, ok := out[req]; !ok {
			if q, ok := out[lim]; ok {
				out[req] = q
			}
		}
		for _, lr := range ranges {
			for _, item := range lr.Spec.Limits {
				if item.Type != corev1.LimitTypeContainer {
					continue
				}
				if _, ok := out[lim]; !ok {
					if q, ok := item.Default[res]; ok {
						out[lim] = q
					}
				}
				if _, ok := out[req]; !ok {
					if q, ok := item.DefaultRequest[res]; ok {
						out[req] = q
					}
				}
			}
		}
	}
	return out
}

// withinLimitRanges checks the containers the fix changes, and their pod,
// against every LimitRange in the namespace.
func withinLimitRanges(ranges []corev1.LimitRange, s planned) error {
	var changed []string
	for _, d := range s.changed {
		if !slices.Contains(changed, d.container) {
			changed = append(changed, d.container)
		}
	}
	pod := map[field]resource.Quantity{}
	for f, v := range podTotals(s.after, ranges) {
		pod[f] = *quantity(v, f.resource)
	}
	for _, lr := range ranges {
		in := fmt.Sprintf("The LimitRange %s in %s", lr.Name, lr.Namespace)
		for _, item := range lr.Spec.Limits {
			switch item.Type {
			case corev1.LimitTypeContainer:
				for _, name := range changed {
					i := slices.IndexFunc(s.after.Spec.Containers, func(c corev1.Container) bool { return c.Name == name })
					if err := bounds(in, "container", name, item, effective(s.after.Spec.Containers[i], ranges)); err != nil {
						return err
					}
				}
			case corev1.LimitTypePod:
				if err := bounds(in, "pod", "each pod", item, pod); err != nil {
					return err
				}
			case corev1.LimitTypePersistentVolumeClaim:
			}
		}
	}
	return nil
}

// bounds checks one container's or pod's values against a LimitRange item.
func bounds(in, per, who string, item corev1.LimitRangeItem, values map[field]resource.Quantity) error {
	refuse := func(format string, args ...any) error {
		return &capability.Refusal{Reason: in + " " + fmt.Sprintf(format, args...) + "."}
	}
	for _, res := range []corev1.ResourceName{corev1.ResourceMemory, corev1.ResourceCPU} {
		name := resourceName(res)
		reqField, limField := field{"requests", res}, field{"limits", res}
		req, hasReq := values[reqField]
		lim, hasLim := values[limField]
		for _, v := range []struct {
			f   field
			q   resource.Quantity
			has bool
		}{{limField, lim, hasLim}, {reqField, req, hasReq}} {
			if m, ok := item.Max[res]; ok && v.has && v.q.Cmp(m) > 0 {
				return refuse("allows at most %s of %s per %s, and %s would have a %s of %s", m.String(), name, per, who, v.f, v.q.String())
			}
			if m, ok := item.Min[res]; ok && v.has && v.q.Cmp(m) < 0 {
				return refuse("allows no less than %s of %s per %s, and %s would have a %s of %s", m.String(), name, per, who, v.f, v.q.String())
			}
		}
		if !hasReq || !hasLim || req.Sign() <= 0 {
			continue
		}
		if req.Cmp(lim) > 0 {
			return refuse("would leave %s with a %s of %s, above its %s of %s", who, reqField, req.String(), limField, lim.String())
		}
		ratio := lim.AsApproximateFloat64() / req.AsApproximateFloat64()
		if m, ok := item.MaxLimitRequestRatio[res]; ok && ratio > m.AsApproximateFloat64() {
			return refuse("allows a %s at most %s times the request, and %s would have %s for a %s request",
				limField, m.String(), who, lim.String(), req.String())
		}
	}
	return nil
}

// fits checks that some node could run a pod that asks for more than before.
// Only nodes the pod could be scheduled on count: schedulable, matching its
// node selector, with no taint it doesn't tolerate. When there are none
// Themis can tell about, it can't judge, so it doesn't refuse.
func fits(nodes []corev1.Node, spec corev1.PodSpec, before, after totals) error {
	for _, res := range []corev1.ResourceName{corev1.ResourceMemory, corev1.ResourceCPU} {
		f := field{"requests", res}
		if after[f] <= before[f] {
			continue
		}
		largest, found := int64(0), false
		for _, n := range nodes {
			if !schedulable(n, spec) {
				continue
			}
			q := n.Status.Allocatable[res]
			largest, found = max(largest, q.MilliValue()), true
		}
		if found && after[f] > largest {
			return &capability.Refusal{Reason: fmt.Sprintf("No node can fit a pod with a %s %s; the largest has %s allocatable.",
				quantity(after[f], res), f, quantity(largest, res))}
		}
	}
	return nil
}

func schedulable(n corev1.Node, spec corev1.PodSpec) bool {
	if n.Spec.Unschedulable || !labels.SelectorFromSet(spec.NodeSelector).Matches(labels.Set(n.Labels)) {
		return false
	}
	for _, taint := range n.Spec.Taints {
		if taint.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		if !slices.ContainsFunc(spec.Tolerations, func(t corev1.Toleration) bool {
			return t.ToleratesTaint(logr.Discard(), &taint, false)
		}) {
			return false
		}
	}
	return true
}

// quotaKeys are the ResourceQuota entries a pod's requests and limits count
// towards.
var quotaKeys = map[corev1.ResourceName]field{
	corev1.ResourceRequestsMemory: {"requests", corev1.ResourceMemory},
	corev1.ResourceMemory:         {"requests", corev1.ResourceMemory},
	corev1.ResourceLimitsMemory:   {"limits", corev1.ResourceMemory},
	corev1.ResourceRequestsCPU:    {"requests", corev1.ResourceCPU},
	corev1.ResourceCPU:            {"requests", corev1.ResourceCPU},
	corev1.ResourceLimitsCPU:      {"limits", corev1.ResourceCPU},
}

// withinQuotas checks the namespace's ResourceQuotas leave room for the
// rollout. At its peak, surge new pods run beside the old ones: it needs
// replicas × (new − old) + surge × old more, or surge × new while the first
// new pods start, whichever is more. Quotas scoped to some pods only are
// left out, since Themis can't tell which pods they count.
func withinQuotas(quotas []corev1.ResourceQuota, before, after totals, replicas, surge int64) error {
	for _, q := range quotas {
		if len(q.Spec.Scopes) > 0 || q.Spec.ScopeSelector != nil {
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(quotaKeys)) {
			hard, ok := q.Spec.Hard[key]
			f := quotaKeys[key]
			old, now := before[f], after[f]
			if !ok || old == now {
				continue
			}
			need := max(surge*now, replicas*(now-old)+surge*old)
			used := q.Status.Used[key]
			if need > 0 && used.MilliValue()+need > hard.MilliValue() {
				return &capability.Refusal{Reason: fmt.Sprintf(
					"The ResourceQuota %s in %s allows %s of %ss; %s is in use, and rolling out the change needs %s more.",
					q.Name, q.Namespace, hard.String(), f, quantity(used.MilliValue(), f.resource), quantity(need, f.resource))}
			}
		}
	}
	return nil
}

// surge is how many new pods a rollout of the workload runs beside the old
// ones: none when it recreates them.
func surge(s planned, replicas int32) int64 {
	strategy := s.workload.Object().Spec.Strategy
	if strategy.Type == "Recreate" {
		return 0
	}
	value := intstr.FromString("25%")
	if strategy.RollingUpdate != nil && strategy.RollingUpdate.MaxSurge != nil {
		value = *strategy.RollingUpdate.MaxSurge
	}
	n, err := intstr.GetScaledValueFromIntOrPercent(&value, int(replicas), true)
	if err != nil {
		return 0
	}
	return int64(n)
}

// quantity shows thousandths of a unit of res as Kubernetes writes them.
func quantity(milli int64, res corev1.ResourceName) *resource.Quantity {
	format := resource.BinarySI
	if res == corev1.ResourceCPU {
		format = resource.DecimalSI
	}
	return resource.NewMilliQuantity(milli, format)
}
