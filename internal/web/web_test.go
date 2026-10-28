package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/web"
)

func TestServesThePage(t *testing.T) {
	mux := http.NewServeMux()
	web.Register(mux)
	for path, want := range map[string]string{"/": "<title>Themis</title>", "/app.js": "/v1/incidents", "/style.css": "--accent"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d, missing %q", path, rec.Code, want)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("GET %s has no script policy", path)
		}
	}
}
