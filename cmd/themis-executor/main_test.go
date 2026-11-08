package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/executor"
)

func TestExecutorMetrics_Served(t *testing.T) {
	newExecutor(nil, nil, executor.Scope{}, nil)
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

// The executor doesn't start without namespaces to change, or with a fix it
// doesn't have, before it connects to anything.
func TestRun_RefusesAScopeItCantHonour(t *testing.T) {
	var scope executor.Scope
	if err := list(&scope.Namespaces)("shop, payments,,"); err != nil || !slices.Equal(scope.Namespaces, []string{"shop", "payments"}) {
		t.Fatalf("--namespaces read as %q, %v", scope.Namespaces, err)
	}
	for name, tt := range map[string]struct {
		scope executor.Scope
		want  string
	}{
		"no namespaces": {executor.Scope{}, "--namespaces is empty"},
		"unknown fix":   {executor.Scope{Namespaces: []string{"shop"}, Fixes: []string{"deployment.delete"}}, "--fixes names deployment.delete, which isn't a fix Themis has"},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(t.Context(), slog.Default(), tt.scope, "", "", "", "", "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("run = %v, want %q", err, tt.want)
			}
		})
	}
}
