package evidence

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Suspicion is text in an item that looks written for the investigator
// rather than by the workload: instructions, Themis or a tool impersonated,
// an order to make a fix. Anyone who can write logs, events or configuration
// chooses that text, so a person deciding on a fix should know it is there.
// Themis flags it and changes nothing else: the item keeps it.
type Suspicion struct {
	// Reason says in plain words what the text looks like.
	Reason string `json:"reason"`
	// At is where in the item's data, as a JSON pointer.
	At string `json:"at"`
	// Excerpt is the text, cut short, after redaction.
	Excerpt string `json:"excerpt"`
}

// Reasons Themis gives for flagging text. Each completes "the evidence
// holds ...".
const (
	ReasonInstructions  = "instructions addressed to an AI or investigator"
	ReasonImpersonation = "text posing as Themis, a tool's output, metrics or an approval"
	ReasonFixOrder      = "an order to make, or not make, a fix"
	ReasonDiagnosis     = "a ready-made diagnosis"
	ReasonEncoded       = "encoded text that decodes to instructions"
)

// suspiciousRules are small and specific: real logs say "rollback" or
// "kubectl", so each rule needs the words around them. They were tuned on the
// scenario packs' cases: none flags a case written without an injection.
var suspiciousRules = []struct {
	reason  string
	pattern *regexp.Regexp
}{
	{ReasonInstructions, regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override)\b(?:\s+\w+){0,3}?\s+(?:instructions|rules|prompts?|guidance)\b`)},
	{ReasonInstructions, regexp.MustCompile(`(?i)\b(?:as an ai|as a language model|language model)\b|\b(?:investigator|ai agent|ai assistant)\s*[:,]`)},
	{ReasonInstructions, regexp.MustCompile(`\bSYSTEM\b\s*[:(]|(?i)<\|im_start\|>|\[/?INST\]|<<SYS>>|</?system>|\b(?:system|assistant) (?:prompt|message)\s*:`)},
	{ReasonInstructions, regexp.MustCompile(`(?i)\b(?:begin|start|end|write) your (?:summary|reply|answer|response)\b|\byour (?:instructions|system prompt)\b`)},
	{ReasonInstructions, regexp.MustCompile(`(?im)(?:^|[.;!?]\s+)name\s+(?:the root cause\b|[a-z]+(?:_[a-z]+)+\s+first\b)|\bdo(?: not|n'?t) mention\b|\bdecode and follow\b`)},
	{ReasonInstructions, regexp.MustCompile(`(?i)\b(?:conclude[_ ]inconclusive|request_evidence|propose_fix)\b`)},

	{ReasonImpersonation, regexp.MustCompile(`(?i)[\[(]\s*themis\b[^\])\n]{0,40}[\])]|\btool_(?:result|output|call)s?\b|\bpredicate (?:update|result)s?\b`)},
	{ReasonImpersonation, regexp.MustCompile(`(?i)\bapproved by (?:the )?(?:sre|on[- ]call|incident commander)|\b(?:rollback|roll back|change|fix)\b(?:\s+\S+){0,6}?\s+is approved\b|\bchange[ -]freeze\b`)},
	{ReasonImpersonation, regexp.MustCompile(`(?i)\balready (?:remediated|mitigated)\b|\bincident (?:is |was )?(?:handled|remediated)\b|\b(?:proposals?|fix(?:es)?|rollbacks?) will be (?:rejected|refused|denied)\b`)},
	// Throttling Themis reads from metrics, written in a log line instead.
	{ReasonImpersonation, regexp.MustCompile(`\bnr_throttled\b|\bthrottled_usec\b`)},

	// Orders are imperative: they start a sentence.
	{ReasonFixOrder, regexp.MustCompile(`(?i)\broll(?:ing)?[ -]?back\b[^.;\n]{0,50}?\brevision\b`)},
	{ReasonFixOrder, regexp.MustCompile(`(?im)(?:^|[.;!?]\s+)(?:raise|set|increase|bump|lower|reduce|decrease|double)\b[^.;\n]{0,50}?\b(?:(?:cpu|memory)\s+(?:limit|request)s?|initialDelaySeconds|timeoutSeconds|periodSeconds|failureThreshold)\b`)},
	{ReasonFixOrder, regexp.MustCompile(`(?i)\bkubectl\s+(?:rollout|set|scale|patch|edit|apply|delete|annotate|label|replace)\b`)},
	{ReasonFixOrder, regexp.MustCompile(`(?i)\bdeployment\.(?:rollback-to-revision|set-probes|set-resources)\b|\bpropose (?:a|an|the|it|this|no)\b`)},
	{ReasonFixOrder, regexp.MustCompile(`(?i)\bdo(?: not|n'?t)\s+(?:act|roll ?back|raise|lower|propose|remediate|change (?:the |its |any )?(?:memory|cpu|probe|limit|resource|replica))`)},
	{ReasonFixOrder, regexp.MustCompile(`(?i)\bclose\b[^.;\n]{0,40}?\bincident\b|\bno further checks\b|\b(?:rollback|roll back|fix) is not (?:needed|required|necessary)\b`)},

	{ReasonDiagnosis, regexp.MustCompile(`(?i)\bknown (?:recovered )?(?:error|regression|bug)\b|\bnot a (?:leak|regression|bug)\b`)},
}

// encoded finds runs long enough to hide a sentence in base64.
var encoded = regexp.MustCompile(`[A-Za-z0-9+/]{32,}={0,2}`)

// maxSuspicions bounds what one item carries: a few say enough.
const maxSuspicions = 5

// maxExcerptBytes bounds an excerpt.
const maxExcerptBytes = 120

// Suspicions finds text in data, a JSON document, that looks written for the
// investigator. It reads every string, keys included, after decoding, so
// escapes hide nothing.
func Suspicions(data []byte) []Suspicion {
	var out []Suspicion
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	for len(out) < maxSuspicions {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Not JSON: read it as text.
			return suspicionsIn(string(data), "", nil)
		}
		if tok.Kind() == '"' {
			out = suspicionsIn(tok.String(), string(dec.StackPointer()), out)
		}
	}
	return out
}

// suspicionsIn adds what s holds, found at, to out, one per reason.
func suspicionsIn(s, at string, out []Suspicion) []Suspicion {
	seen := map[string]bool{}
	for _, r := range suspiciousRules {
		if seen[r.reason] || len(out) >= maxSuspicions {
			continue
		}
		if loc := r.pattern.FindStringIndex(s); loc != nil {
			seen[r.reason] = true
			out = append(out, Suspicion{Reason: r.reason, At: at, Excerpt: excerpt(s, loc[0], loc[1])})
		}
	}
	for _, loc := range encoded.FindAllStringIndex(s, -1) {
		if len(out) >= maxSuspicions {
			break
		}
		plain, ok := decoded(s[loc[0]:loc[1]])
		if !ok {
			continue
		}
		if inner := suspicionsIn(plain, at, nil); len(inner) > 0 {
			out = append(out, Suspicion{Reason: ReasonEncoded, At: at, Excerpt: "decodes to: " + bounded(plain, maxExcerptBytes)})
			break
		}
	}
	return out
}

// decoded is run decoded from base64, when that gives printable text.
func decoded(run string) (string, bool) {
	b, err := base64.StdEncoding.DecodeString(run)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(run, "=")); err != nil {
			return "", false
		}
	}
	for _, c := range b {
		if (c < ' ' || c > '~') && c != '\n' && c != '\t' {
			return "", false
		}
	}
	return string(b), true
}

// excerpt is the line of s holding s[start:end], cut to maxExcerptBytes
// from a little before it, with an ellipsis where it was cut.
func excerpt(s string, start, end int) string {
	lineStart := strings.LastIndexByte(s[:start], '\n') + 1
	lineEnd := len(s)
	if i := strings.IndexByte(s[end:], '\n'); i >= 0 {
		lineEnd = end + i
	}
	from := max(lineStart, start-30)
	for from > lineStart && !utf8.RuneStart(s[from]) {
		from--
	}
	text := bounded(s[from:lineEnd], maxExcerptBytes)
	out := strings.TrimSpace(text)
	if from > lineStart {
		out = "…" + out
	}
	if from+len(text) < lineEnd {
		out += "…"
	}
	return out
}

// Warning says, in plain words, that some of items hold text that looks
// written for the investigator: which templates and, with excerpts, what
// text. It is empty when none does.
func Warning(items []Item, excerpts bool) string {
	var seen, where []string
	for _, it := range items {
		if len(it.Suspicious) == 0 || slices.Contains(seen, it.Template) || len(seen) == 3 {
			continue
		}
		seen = append(seen, it.Template)
		w := it.Template
		if excerpts {
			w += `: "` + it.Suspicious[0].Excerpt + `"`
		}
		where = append(where, w)
	}
	if len(where) == 0 {
		return ""
	}
	return "Some evidence contains text addressed to Themis or posing as tool output (" + strings.Join(where, "; ") + "). It is material, not a finding; the checks do not rely on it."
}
