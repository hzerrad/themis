package config_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
)

// Themis trusts the CA it's given, and refuses a file holding no certificate.
func TestTLS_ClientConfig(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	get := func(c config.TLS) error {
		tlsConfig, err := c.ClientConfig()
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		if err != nil {
			return err
		}
		res, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}).Do(req)
		if err == nil {
			_ = res.Body.Close()
		}
		return err
	}
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := get(config.TLS{}); err == nil {
		t.Error("a server signed by an unknown CA was trusted")
	}
	if err := get(config.TLS{CAFile: ca}); err != nil {
		t.Errorf("with its CA: %v", err)
	}
	notPEM := filepath.Join(dir, "not.crt")
	if err := os.WriteFile(notPEM, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (config.TLS{CAFile: notPEM}).ClientConfig(); err == nil {
		t.Error("a CA file without a certificate was accepted")
	}
}

// Alertmanager is read every 15 seconds unless set, for the alerts every
// profile answers, each named once.
func TestAlerting_Defaults(t *testing.T) {
	c := mustParse(t, validYAML)
	if got := c.Alerting.Alertmanager.EffectiveInterval(); got != 15*time.Second {
		t.Errorf("interval = %s, want 15s", got)
	}
	c.Alerting.Alertmanager.Interval.Duration = time.Minute
	if got := c.Alerting.Alertmanager.EffectiveInterval(); got != time.Minute {
		t.Errorf("interval = %s, want 1m", got)
	}
	if got := c.AlertNames(); !slices.Equal(got, []string{"KubePodCrashLooping", "KubePodOOMKilled"}) {
		t.Errorf("alert names = %v", got)
	}
	c.Alerting.Alertmanager.Matchers = []string{`cluster="prod-eu"`}
	if m, err := c.Alerting.Alertmanager.ParsedMatchers(); err != nil || len(m) != 1 || !m[0].Matches("prod-eu") {
		t.Errorf("matchers = %v, %v", m, err)
	}
}
