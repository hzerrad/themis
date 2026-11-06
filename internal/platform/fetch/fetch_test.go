package fetch_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/platform/fetch"
)

func file(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The credentials are read from their files and sent with each request.
func TestGet_Credentials(t *testing.T) {
	for name, tc := range map[string]struct {
		creds fetch.Credentials
		want  string
	}{
		"none":       {fetch.Credentials{}, ""},
		"a token":    {fetch.Credentials{BearerTokenFile: file(t, "s3cret\n")}, "Bearer s3cret"},
		"basic auth": {fetch.Credentials{Username: "themis", PasswordFile: file(t, "s3cret")}, "Basic dGhlbWlzOnMzY3JldA=="},
	} {
		t.Run(name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				_, _ = w.Write([]byte(`{"ok": true}`))
			}))
			defer srv.Close()
			c, err := fetch.New("Alertmanager", srv.URL, tc.creds, nil, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				OK bool `json:"ok"`
			}
			if err := c.Get(t.Context(), "api/v2/status", nil, 1<<10, &out); err != nil || !out.OK {
				t.Fatalf("Get = %+v, %v", out, err)
			}
			if got != tc.want {
				t.Errorf("Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}

// Each failure says what the server did, with its name and address, and of
// which kind it is, for the remedy.
func TestGet_Failures(t *testing.T) {
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		closed  bool
		kind    string
		says    string
	}{
		"unreachable":  {closed: true, kind: fetch.Unreachable, says: "Alertmanager at %s doesn't answer: dial tcp"},
		"too slow":     {handler: func(http.ResponseWriter, *http.Request) { time.Sleep(300 * time.Millisecond) }, kind: fetch.Unreachable, says: "Alertmanager at %s didn't answer within 100ms"},
		"refused":      {handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }, kind: fetch.Refused, says: "Alertmanager at %s answered 403 Forbidden"},
		"broken":       {handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, kind: fetch.Unexpected, says: "Alertmanager at %s answered 502 Bad Gateway"},
		"not its JSON": {handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }, kind: fetch.Unexpected, says: "the answer from %s isn't Alertmanager's"},
		"too large": {handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`"` + strings.Repeat("a", 2<<20) + `"`))
		}, kind: fetch.Unexpected, says: "Alertmanager at %s answered with more than 1 MiB"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			if tc.closed {
				srv.Close()
			}
			c, err := fetch.New("Alertmanager", srv.URL, fetch.Credentials{}, nil, 100*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			var out any
			err = c.Get(t.Context(), "api/v2/alerts", nil, 1<<20, &out)
			says := strings.ReplaceAll(tc.says, "%s", srv.URL)
			if fetch.KindOf(err) != tc.kind || err == nil || !strings.HasPrefix(err.Error(), says) {
				t.Errorf("Get = %v (%s), want %s saying %q", err, fetch.KindOf(err), tc.kind, says)
			}
		})
	}
	if _, err := fetch.New("Alertmanager", "http://%zz", fetch.Credentials{}, nil, time.Second); err == nil {
		t.Error("an address that isn't a URL was accepted")
	}
	c, _ := fetch.New("Alertmanager", "http://127.0.0.1:1", fetch.Credentials{BearerTokenFile: filepath.Join(t.TempDir(), "missing")}, nil, time.Second)
	if err := c.Get(t.Context(), "x", nil, 1, new(any)); err == nil || !strings.HasPrefix(err.Error(), "Alertmanager's token") {
		t.Errorf("a missing token file: %v", err)
	}
}
