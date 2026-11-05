package metrics

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Kubernetes is the source named in evidence for the metrics API.
const Kubernetes = "kubernetes"

// Why the metrics API can't answer.
const (
	noHistory   = "the Kubernetes metrics API keeps no history, so it has no peaks or throttling: set a Prometheus address to see them"
	notServed   = "the Kubernetes metrics API isn't served in this cluster: install metrics-server, or set a Prometheus address"
	notMeasured = "the Kubernetes metrics API has no figures for these pods yet"
)

// metricsAPI reads the metrics API (metrics.k8s.io), which metrics-server
// serves: what each container uses now, and nothing older.
type metricsAPI struct {
	client metricsclient.Interface
}

// NewKubernetes returns the metrics API as a source.
func NewKubernetes(c metricsclient.Interface) Source { return metricsAPI{client: c} }

func (metricsAPI) Name() string { return Kubernetes }

func (k metricsAPI) Memory(ctx context.Context, pods Pods) (Reading, error) {
	return k.read(ctx, pods, func(c metricsv1beta1.ContainerMetrics) float64 {
		return float64(c.Usage.Memory().Value())
	})
}

func (k metricsAPI) CPU(ctx context.Context, pods Pods) (Reading, error) {
	return k.read(ctx, pods, func(c metricsv1beta1.ContainerMetrics) float64 {
		return float64(c.Usage.Cpu().MilliValue()) / 1000
	})
}

func (metricsAPI) PeakMemory(context.Context, Pods, time.Duration) (Reading, error) {
	return Reading{Unavailable: noHistory}, nil
}

func (metricsAPI) Throttling(context.Context, Pods, time.Duration) (Reading, error) {
	return Reading{Unavailable: noHistory}, nil
}

// read lists the namespace's pod metrics, keeping the highest value of each
// container among pods.
func (k metricsAPI) read(ctx context.Context, pods Pods, value func(metricsv1beta1.ContainerMetrics) float64) (Reading, error) {
	list, err := k.client.MetricsV1beta1().PodMetricses(pods.Namespace).List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Reading{Unavailable: notServed}, nil
	case err != nil:
		return Reading{}, fmt.Errorf("list pod metrics in %s: %w", pods.Namespace, err)
	}
	r := Reading{Containers: map[string]float64{}}
	for _, pm := range list.Items {
		if !slices.Contains(pods.Names, pm.Name) {
			continue
		}
		for _, c := range pm.Containers {
			if v, seen := r.Containers[c.Name]; !seen || value(c) > v {
				r.Containers[c.Name] = value(c)
			}
		}
	}
	if len(r.Containers) == 0 {
		r.Unavailable = notMeasured
	}
	return r, nil
}

// probe asks whether the cluster serves the metrics API, which needs no
// permission.
func (k metricsAPI) probe(ctx context.Context) error {
	gv := metricsv1beta1.SchemeGroupVersion.String()
	resources, err := k.client.Discovery().ServerResourcesForGroupVersionWithContext(ctx, gv)
	if apierrors.IsNotFound(err) || (err == nil && !slices.ContainsFunc(resources.APIResources, func(r metav1.APIResource) bool { return r.Name == "pods" })) {
		return errors.New(notServed)
	}
	return err
}

func (metricsAPI) history() bool { return false }
