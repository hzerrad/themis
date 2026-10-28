package evidence

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"regexp"
)

// Rule replaces matches of Pattern with Replace, which may refer to groups.
type Rule struct {
	Name    string
	Pattern *regexp.Regexp
	Replace string
}

const redacted = "[REDACTED]"

// DefaultRules redact common secrets in plain text.
var DefaultRules = []Rule{
	// A truncated log can cut a key's header or footer off.
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`), redacted},
	{"private-key-tail", regexp.MustCompile(`(?s)\A.*?-----END [A-Z ]*PRIVATE KEY-----`), redacted},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), redacted},
	{"authorization", regexp.MustCompile(`(?i)\b((?:bearer|basic)\s+)[A-Za-z0-9\-._~+/]+=*`), "${1}" + redacted},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), redacted},
	{"github-token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`), redacted},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), redacted},
	{"url-credentials", regexp.MustCompile(`([a-z][a-z0-9+.-]*://[^\s:@/]+:)[^\s@/]+@`), "${1}" + redacted + "@"},
	{"key-value", regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|token|cookie|session)(?:\\?["'])?\s*[:=]\s*(?:\\?["'])?)[^\s"'\\,;&]+`), "${1}" + redacted},
}

// Redact applies rules to every string of a JSON document, after decoding, so
// escapes cannot hide a secret. Anything else is redacted as plain text. It
// returns the result with the number of replacements.
func Redact(data []byte, rules []Rule) ([]byte, int) {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	var out bytes.Buffer
	enc := jsontext.NewEncoder(&out)
	n := 0
	for {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return redactText(data, rules)
		}
		if tok.Kind() == '"' {
			if s, k := redactText([]byte(tok.String()), rules); k > 0 {
				tok, n = jsontext.String(string(s)), n+k
			}
		}
		if err := enc.WriteToken(tok); err != nil {
			return redactText(data, rules)
		}
	}
	if n == 0 {
		return data, 0
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), n
}

func redactText(data []byte, rules []Rule) ([]byte, int) {
	n := 0
	for _, r := range rules {
		data = r.Pattern.ReplaceAllFunc(data, func(m []byte) []byte {
			n++
			return r.Pattern.Expand(nil, []byte(r.Replace), m, r.Pattern.FindSubmatchIndex(m))
		})
	}
	return data, n
}
