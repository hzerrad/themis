package grpcapi_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/server/grpcapi"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

func TestMetrics_TokenReviews(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	accepted := metrictest.Delta(t, "themis_token_reviews_total", map[string]string{"result": "accepted"})
	refused := metrictest.Delta(t, "themis_token_reviews_total", map[string]string{"result": "refused"})
	hits := metrictest.Delta(t, "themis_token_review_cache_hits_total", nil)
	limited := metrictest.Delta(t, "themis_requests_limited_total", map[string]string{"kind": "reads"})
	for range grpcapi.ReadsPerMinute + 1 {
		_ = list(t, agent)
	}
	_ = list(t, e.clientWith(t, "forged"))
	if a, r, h, l := accepted(), refused(), hits(), limited(); a != 1 || r != 1 || h != grpcapi.ReadsPerMinute || l != 1 {
		t.Errorf("accepted %v, refused %v, cache hits %v, limited %v", a, r, h, l)
	}
}
