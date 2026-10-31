package web_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
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

// The UI must not answer for the API: a wrong method gets 405, not a page.
func TestLeavesOtherPathsToTheAPI(t *testing.T) {
	mux := http.NewServeMux()
	web.Register(mux)
	mux.Handle("POST /v1/intake/alertmanager", http.NotFoundHandler())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/intake/alertmanager", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a POST route = %d, want 405", rec.Code)
	}
}

// internalWords are Themis's own terms. The page speaks to operators, who read
// it under pressure: it uses plain words, and Kubernetes' where they fit.
var internalWords = []string{
	"governance", "governed", "predicate", "hypothes", "capability", "digest", "lease",
	"blast radius", "safety event", "abstain", "proposal", "remediation", "derived",
}

// literal matches the string literals of the script, and the text of the page.
var literal = regexp.MustCompile("\"[^\"\\n]*\"|`[^`]*`|>[^<]+<")

// interpolation matches the code inside a template literal.
var interpolation = regexp.MustCompile(`\$\{[^}]*\}`)

func TestPage_SpeaksPlainly(t *testing.T) {
	mux := http.NewServeMux()
	web.Register(mux)
	for _, path := range []string{"/", "/app.js"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		for _, lit := range literal.FindAllString(rec.Body.String(), -1) {
			if strings.HasPrefix(lit, "\"/") || strings.HasPrefix(lit, "`/") {
				continue // API paths
			}
			text := interpolation.ReplaceAllString(lit, "")
			for _, w := range internalWords {
				if strings.Contains(strings.ToLower(text), w) {
					t.Errorf("%s shows %q, which uses the internal word %q", path, lit, w)
				}
			}
		}
	}
}
