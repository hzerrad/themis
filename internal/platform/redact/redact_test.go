package redact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/platform/redact"
)

func TestApply_Secrets(t *testing.T) {
	tests := map[string]struct {
		in, secret string
	}{
		"bearer token":     {`calling api with Authorization: Bearer abc.DEF-123_xyz`, "abc.DEF-123_xyz"},
		"jwt":              {`token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.c2lnbmF0dXJl rejected`, "eyJhbGciOiJIUzI1NiJ9"},
		"aws access key":   {`using key AKIAIOSFODNN7EXAMPLE for s3`, "AKIAIOSFODNN7EXAMPLE"},
		"password field":   {`connect: password=hunter2 host=db`, "hunter2"},
		"quoted secret":    {`{\"api_key\": \"sk-live-1234567890\"}`, "sk-live-1234567890"},
		"url credentials":  {`dial postgres://app:s3cr3tpass@db:5432/shop`, "s3cr3tpass"},
		"private key":      {"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEA"},
		"client secret":    {`client_secret: 9f8e7d6c5b4a`, "9f8e7d6c5b4a"},
		"github token":     {`cloning with ghp_abcdefghijklmnopqrstuvwxyz0123456789`, "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		"slack token":      {`posting with xoxb-1234-5678-abcdefghij`, "xoxb-1234-5678-abcdefghij"},
		"case insensitive": {`PASSWORD: Hunter2!`, "Hunter2!"},
		"basic auth":       {`Authorization: Basic dXNlcjpodW50ZXIy`, "dXNlcjpodW50ZXIy"},
		"github pat":       {`token github_pat_11ABCDEFG0123456789_abcdefghijklmnop`, "github_pat_11ABCDEFG0123456789_abcdefghijklmnop"},
		"cookie":           {`Set-Cookie: session=7f3a9c2e1b; Path=/`, "7f3a9c2e1b"},
		"tab separator":    {"password:\thunter2", "hunter2"},
		"headless key":     {"MIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----\nserving", "MIIEowIBAAKCAQEA"},
		"unterminated key": {"-----BEGIN PRIVATE KEY-----\nMIIEowIBAAKCAQEA", "MIIEowIBAAKCAQEA"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(map[string]string{"logs": tt.in})
			got, n := redact.Apply(data, redact.DefaultRules)
			if strings.Contains(string(got), tt.secret) {
				t.Errorf("Apply kept %q: %s", tt.secret, got)
			}
			if n == 0 {
				t.Error("Apply counted no redactions")
			}
			if !json.Valid(got) {
				t.Errorf("Apply broke the JSON: %s", got)
			}
		})
	}
}

// JSON escapes must not hide a secret from rules written for plain text.
func TestApply_SeesThroughJSONEscapes(t *testing.T) {
	data := []byte(`{"logs":"password=\u0068unter2 and Bearer\tabcdef123456"}`)
	got, _ := redact.Apply(data, redact.DefaultRules)
	for _, secret := range []string{"unter2", "abcdef123456"} {
		if strings.Contains(string(got), secret) {
			t.Errorf("Apply kept %q: %s", secret, got)
		}
	}
}

func TestApply_KeepsNumbersAndOrder(t *testing.T) {
	data := []byte(`{"z":"password=x","a":9007199254740993}`)
	got, _ := redact.Apply(data, redact.DefaultRules)
	if want := `{"z":"password=[REDACTED]","a":9007199254740993}`; string(got) != want {
		t.Errorf("Apply = %s, want %s", got, want)
	}
}

func TestApply_LeavesOrdinaryTextAlone(t *testing.T) {
	data := []byte(`{"logs":"OOMKilled: container app used 64Mi; password reset page served"}`)
	got, n := redact.Apply(data, redact.DefaultRules)
	if string(got) != string(data) || n != 0 {
		t.Errorf("Apply = %s, %d; want the input unchanged", got, n)
	}
}
