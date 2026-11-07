package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/server/httpapi"
)

// After an upgrade the browser must load the new page: it revalidates every
// time, and an unchanged file costs a 304.
func TestServesThePage(t *testing.T) {
	mux := http.NewServeMux()
	httpapi.RegisterUI(mux)
	pages := map[string]string{
		"/": "<title>Themis</title>", "/app.js": "/v1/incidents", "/style.css": "--verdigris",
		"/themis.svg": "<svg", "/fonts/public-sans-latin-400-normal.woff2": "wOF2",
	}
	for path, want := range pages {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d, missing %q", path, rec.Code, want)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("GET %s has no script policy", path)
		}
		tag := rec.Header().Get("ETag")
		if rec.Header().Get("Cache-Control") != "no-cache" || tag == "" {
			t.Fatalf("GET %s: Cache-Control %q, ETag %q", path, rec.Header().Get("Cache-Control"), tag)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Header.Set("If-None-Match", tag)
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("GET %s with its own tag = %d, want 304", path, rec.Code)
		}
	}
}

// The fonts are served one by one: their folder is not a listing.
func TestServesNoFolderListing(t *testing.T) {
	mux := http.NewServeMux()
	httpapi.RegisterUI(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/fonts/", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("GET /fonts/ = %d, want no listing", rec.Code)
	}
}

// The UI must not answer for the API: a wrong method gets 405, not a page.
func TestLeavesOtherPathsToTheAPI(t *testing.T) {
	mux := http.NewServeMux()
	httpapi.RegisterUI(mux)
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

// kubernetesWords hold an internal word in Kubernetes' own sense: an image
// digest is what a container runs, not the digest an approval binds.
var kubernetesWords = []string{"image digest"}

// literal matches the string literals of the script, and the text of the page.
var literal = regexp.MustCompile("\"[^\"\\n]*\"|`[^`]*`|>[^<]+<")

// interpolation matches the code inside a template literal.
var interpolation = regexp.MustCompile(`\$\{[^}]*\}`)

func TestPage_SpeaksPlainly(t *testing.T) {
	mux := http.NewServeMux()
	httpapi.RegisterUI(mux)
	for _, path := range []string{"/", "/app.js"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		for _, lit := range literal.FindAllString(rec.Body.String(), -1) {
			if strings.HasPrefix(lit, "\"/") || strings.HasPrefix(lit, "`/") {
				continue // API paths
			}
			text := strings.ToLower(interpolation.ReplaceAllString(lit, ""))
			for _, k := range kubernetesWords {
				text = strings.ReplaceAll(text, k, "")
			}
			for _, w := range internalWords {
				if strings.Contains(strings.ToLower(text), w) {
					t.Errorf("%s shows %q, which uses the internal word %q", path, lit, w)
				}
			}
		}
	}
}
