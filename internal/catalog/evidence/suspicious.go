package evidence

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"regexp"
	"strconv"
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

// what says, for people, what text flagged for each reason is: "The line is
// written as an instruction to Themis." The reasons themselves are sent to
// the investigator as they are.
var what = map[string]string{
	ReasonInstructions:  "is written as an instruction to Themis",
	ReasonImpersonation: "is made to look like output from Themis or a tool, or like an approval",
	ReasonFixOrder:      "is written as an order to make or skip a fix",
	ReasonDiagnosis:     "offers a ready-made diagnosis",
	ReasonEncoded:       "holds encoded instructions",
}

// What says what the flagged text is, to follow its subject, such as "is
// written as an instruction to Themis".
func (s Suspicion) What() string {
	if w, ok := what[s.Reason]; ok {
		return w
	}
	return "looks written for Themis rather than by the workload"
}

// kinds name each template's evidence inside a sentence.
var kinds = map[string]string{
	ContainerLogsTail: "the container logs",
	TargetEvents:      "the events",
	PodStates:         "the pod states",
	ReferencedConfig:  "the referenced config",
	RolloutHistory:    "the rollout history",
}

// subject names the flagged text by what holds it: a line of logs, an
// event's message, or text.
func (it Item) subject(s Suspicion) string {
	switch {
	case it.Template == ContainerLogsTail:
		return "line"
	case it.Template == TargetEvents && strings.HasSuffix(s.At, "/message"):
		return "event message"
	}
	return "text"
}

// Origin names, in the cluster's words, what in it holds s: the pod and
// container whose logs hold it, the event and the object it is about, the
// pod or container whose state does, the ConfigMap or Secret, the revision.
// It is empty when the data doesn't say.
func (it Item) Origin(s Suspicion) string {
	var data map[string]any
	if json.Unmarshal(it.Data, &data) != nil {
		return ""
	}
	path := strings.Split(strings.TrimPrefix(s.At, "/"), "/")
	text := func(m map[string]any, key string) string {
		v, _ := m[key].(string)
		return v
	}
	switch it.Template {
	case ContainerLogsTail:
		pod, container := text(data, "pod"), text(data, "container")
		if pod == "" {
			return ""
		}
		out := "pod " + pod
		if container != "" {
			out += ", container " + container
		}
		if previous, _ := data["previousRun"].(bool); previous {
			out += ", previous run"
		}
		return out
	case TargetEvents:
		ev := element(data, path, "events")
		if ev == nil {
			return ""
		}
		out := "event"
		if r := text(ev, "reason"); r != "" {
			out = r + " event"
		}
		if o := text(ev, "object"); o != "" {
			out += " on " + o
		}
		return out
	case PodStates:
		pod := element(data, path, "pods")
		if pod == nil || text(pod, "name") == "" {
			return ""
		}
		out := "pod " + text(pod, "name")
		if len(path) > 2 && strings.HasSuffix(path[2], "ontainers") {
			if c := element(pod, path[2:], path[2]); c != nil && text(c, "name") != "" {
				out += ", container " + text(c, "name")
			}
		}
		return out
	case ReferencedConfig:
		ref := element(data, path, "refs")
		if ref == nil || text(ref, "name") == "" {
			return ""
		}
		return strings.TrimSpace(text(ref, "kind") + " " + text(ref, "name"))
	case RolloutHistory:
		rev := element(data, path, "revisions")
		if rev == nil || text(rev, "revision") == "" {
			return ""
		}
		return "revision " + text(rev, "revision")
	}
	return ""
}

// element is the object at path[1] of the list data holds under key, when
// path starts with key.
func element(data map[string]any, path []string, key string) map[string]any {
	if len(path) < 2 || path[0] != key {
		return nil
	}
	list, _ := data[key].([]any)
	i, err := strconv.Atoi(path[1])
	if err != nil || i < 0 || i >= len(list) {
		return nil
	}
	m, _ := list[i].(map[string]any)
	return m
}

// Warning says, in plain words, which of items hold text that looks written
// for Themis, where, and what it is: "Suspicious text in the container logs
// (pod checkout-7d9f8b6c4-x2kqp, container app). The line is written as an
// instruction to Themis." With excerpts, it quotes the text; without, it
// says where to read it. It is empty when no item holds any.
func Warning(items []Item, excerpts bool) string {
	var out []string
	for _, it := range items {
		if len(it.Suspicious) == 0 || len(out) == 3 {
			continue
		}
		s := it.Suspicious[0]
		kind, ok := kinds[it.Template]
		if !ok {
			kind = "the " + it.Template + " evidence"
		}
		w := "Suspicious text in " + kind
		if o := it.Origin(s); o != "" {
			w += " (" + o + ")"
		}
		if excerpts && s.Excerpt != "" {
			w += `: "` + s.Excerpt + `"`
		}
		out = append(out, w+". The "+it.subject(s)+" "+s.What()+".")
	}
	if len(out) == 0 {
		return ""
	}
	if !excerpts {
		read := "Read it"
		if len(out) > 1 {
			read = "Read them"
		}
		out = append(out, read+" in Themis before acting on this incident.")
	}
	return strings.Join(out, " ")
}
