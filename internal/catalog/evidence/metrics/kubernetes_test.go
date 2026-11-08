package metrics_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
)

// metricsAPI is a cluster serving the metrics API, answering a list of pod
// metrics with list, or failing with err.
func metricsAPI(list *metricsv1beta1.PodMetricsList, err error) *metricsfake.Clientset {
	c := metricsfake.NewSimpleClientset()
	c.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: "metrics.k8s.io/v1beta1", APIResources: []metav1.APIResource{{Name: "pods", Namespaced: true}},
	}}
	c.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != "shop" {
			return true, nil, errors.New("listed another namespace")
		}
		if err != nil {
			return true, nil, err
		}
		return true, list, nil
	})
	return c
}

func podMetrics(name string, usage map[string][2]string) metricsv1beta1.PodMetrics {
	pm := metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name}}
	for c, u := range usage {
		pm.Containers = append(pm.Containers, metricsv1beta1.ContainerMetrics{Name: c, Usage: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(u[0]), corev1.ResourceCPU: resource.MustParse(u[1]),
		}})
	}
	return pm
}

var asked = metrics.Pods{Namespace: "shop", Names: []string{"checkout-a", "checkout-b"}}

// Each container's figure is the highest among the pods asked about; other
// pods of the namespace don't count.
func TestKubernetes_HighestAmongThePodsAsked(t *testing.T) {
	src := metrics.NewKubernetes(metricsAPI(&metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{
		podMetrics("checkout-a", map[string][2]string{"app": {"100Mi", "200m"}}),
		podMetrics("checkout-b", map[string][2]string{"app": {"150Mi", "100m"}, "proxy": {"10Mi", "5m"}}),
		podMetrics("cart-a", map[string][2]string{"app": {"900Mi", "2"}}),
	}}, nil))

	memory, err := src.Memory(t.Context(), asked, time.Hour)
	if err != nil || !memory.Answered() || memory.Containers["app"] != 150<<20 || memory.Containers["proxy"] != 10<<20 || len(memory.Containers) != 2 {
		t.Errorf("Memory = %+v, %v, want app 150Mi and proxy 10Mi", memory, err)
	}
	cpu, err := src.CPU(t.Context(), asked, time.Hour)
	if err != nil || cpu.Containers["app"] != 0.2 {
		t.Errorf("CPU = %+v, %v, want app 0.2 cores", cpu, err)
	}
	// The metrics API keeps no history: it says so rather than guess.
	for name, read := range map[string]func() (metrics.Reading, error){
		"peak":       func() (metrics.Reading, error) { return src.PeakMemory(t.Context(), asked, 0) },
		"throttling": func() (metrics.Reading, error) { return src.Throttling(t.Context(), asked, 0) },
	} {
		if r, err := read(); err != nil || r.Answered() || !strings.Contains(r.Unavailable, "Prometheus") {
			t.Errorf("%s = %+v, %v, want unavailable, pointing at Prometheus", name, r, err)
		}
	}
}

func TestKubernetes_CantAnswer(t *testing.T) {
	gr := schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}
	for name, tc := range map[string]struct {
		list        *metricsv1beta1.PodMetricsList
		err         error
		unavailable string
		forbidden   bool
	}{
		"not served":          {err: apierrors.NewNotFound(gr, ""), unavailable: "install metrics-server"},
		"not measured yet":    {list: &metricsv1beta1.PodMetricsList{}, unavailable: "no figures for these pods yet"},
		"not allowed to read": {err: apierrors.NewForbidden(gr, "", errors.New("no")), forbidden: true},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := metrics.NewKubernetes(metricsAPI(tc.list, tc.err)).Memory(t.Context(), asked, time.Hour)
			if apierrors.IsForbidden(err) != tc.forbidden || (!tc.forbidden && (err != nil || !strings.Contains(r.Unavailable, tc.unavailable))) {
				t.Errorf("Memory = %+v, %v, want unavailable %q or forbidden %v", r, err, tc.unavailable, tc.forbidden)
			}
		})
	}
}
