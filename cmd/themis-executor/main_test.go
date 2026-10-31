package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecutorMetrics_Served(t *testing.T) {
	rec := httptest.NewRecorder()
	metricsHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{
		`themis_actions_total{capability="deployment.rollback-to-revision",result="succeeded"} 0`,
		"themis_incident_transitions_total",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}
