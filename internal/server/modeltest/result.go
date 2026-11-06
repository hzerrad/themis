package modeltest

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
)

// Outcome is what a test found.
type Outcome string

const (
	// Connected means the model answered in the reply format.
	Connected Outcome = "connected"
	// Incomplete means the settings lack what the provider needs, so nothing
	// was sent.
	Incomplete Outcome = "incomplete"
	// Unreachable means no connection could be made to the model.
	Unreachable Outcome = "unreachable"
	// NoAnswer means the model didn't answer in time.
	NoAnswer Outcome = "no_answer"
	// Refused means the provider answered with an HTTP error.
	Refused Outcome = "refused"
	// NotInFormat means the model answered, but not in the reply format.
	NotInFormat Outcome = "not_in_format"
	// CutOff means the reply stopped at the output limit.
	CutOff Outcome = "cut_off"
	// Declined means the model declined to answer.
	Declined Outcome = "declined"
	// Errored means the investigator couldn't make the request.
	Errored Outcome = "error"
	// NoInvestigator means no investigator took the test.
	NoInvestigator Outcome = "no_investigator"
	// NoReport means the investigator took the test but never reported.
	NoReport Outcome = "no_report"
)

// Result is what a test found.
type Result struct {
	Outcome Outcome
	// What was tested, once the investigator's own settings applied: the
	// provider, the address, empty for Anthropic's own, and the model's name.
	Provider, Address, Model string
	// How the reply was asked for: strictly or not, and the schema's dialect.
	StrictOutput  bool
	SchemaDialect string
	// AnsweredBy is the model that answered, as the provider names it.
	AnsweredBy string
	// Status is the provider's HTTP status, when it refused.
	Status int
	// Detail is what the provider or the connection said. Never the key.
	Detail  string
	Latency time.Duration
	// OutputLimit is the limit a reply cut off ran into.
	OutputLimit int
	// Missing lists the settings the provider needs and the test lacked.
	Missing []string
}

// OK says the model answered as Themis needs.
func (r Result) OK() bool { return r.Outcome == Connected }

// Text says what the test found, for the Setup page. byInstall says which
// settings the install made: their remedy is a Helm value.
func (r Result) Text(byInstall func(name string) bool) string {
	where := r.where()
	switch r.Outcome {
	case Connected:
		name := r.AnsweredBy
		if name == "" {
			name = r.Model
		}
		return fmt.Sprintf("Connected. %s answered in %s in the reply format.", name, seconds(r.Latency))
	case Incomplete:
		if len(r.Missing) == 0 {
			return "Not tested. The model's settings are incomplete."
		}
		return "Not tested. Enter " + missing(r.Missing) + " first."
	case Unreachable:
		s := "Failed. Can't connect to " + where
		if d := strings.TrimSuffix(quoted(r.Detail), "."); d != "" {
			s += " (" + lowerFirst(d) + ")"
		}
		if r.Address == "" {
			return s + ". Check that model.egressCIDR allows Anthropic's API."
		}
		return s + ". Check the address, and that model.egressCIDR allows it."
	case NoAnswer:
		return fmt.Sprintf("Failed. No answer from %s within %s.", where, seconds(r.Latency))
	case Refused:
		return r.refused(where, byInstall)
	case NotInFormat:
		return "Failed. The model answered, but not in the reply format. " + r.formatRemedy(byInstall)
	case CutOff:
		return fmt.Sprintf("Failed. The reply was cut off at %d tokens. Raise model.maxOutputTokens for a model that reasons.", r.OutputLimit)
	case Declined:
		return "Failed. The model declined to answer."
	case Errored:
		if d := quoted(r.Detail); d != "" {
			return "Failed. The investigator couldn't make the request: " + d
		}
		return "Failed. The investigator couldn't make the request. Read the themis-intelligence pod's logs."
	case NoInvestigator:
		return "No investigator is connected. Themis can't reach the model without it."
	case NoReport:
		return fmt.Sprintf("Failed. The investigator took the test and reported nothing within %s. Read the themis-intelligence pod's logs.", seconds(r.Latency))
	}
	return "Failed."
}

// refused says what a provider's HTTP error means, and what to do about it.
func (r Result) refused(where string, byInstall func(string) bool) string {
	anthropic := r.ownAddress()
	switch {
	case r.Status == 401:
		return fmt.Sprintf("Failed. %s refused the key (401).", where)
	case r.Status == 404 && anthropic:
		return fmt.Sprintf("Failed. %s has no model named %s (404).", where, r.Model)
	case r.Status == 404 && strings.Contains(strings.ToLower(r.Detail), "model"):
		return fmt.Sprintf("Failed. No model named %s at %s (404).", r.Model, where)
	case r.Status == 404:
		return fmt.Sprintf("Failed. No chat completions at %s (404). Check the address, such as https://api.openai.com/v1.", where)
	}
	s := fmt.Sprintf("Failed. %s refused the request (%d)", where, r.Status)
	if r.Status >= 500 {
		s = fmt.Sprintf("Failed. %s answered with an error (%d)", where, r.Status)
	}
	if d := quoted(r.Detail); d != "" {
		s += ": " + d
	} else {
		s += "."
	}
	if remedy := r.requestRemedy(byInstall); remedy != "" {
		s += " " + remedy
	}
	return s
}

// requestRemedy is the remedy for a request the provider refused, when its
// text names a setting Themis sends.
func (r Result) requestRemedy(byInstall func(string) bool) string {
	text := strings.ToLower(r.Detail)
	switch {
	case r.ownAddress():
		return ""
	case strings.Contains(text, "max_completion_tokens"):
		return "Install with model.outputLimitField=max_completion_tokens."
	case strings.Contains(text, "temperature"):
		return "Install with model.temperature=default."
	case r.StrictOutput && (strings.Contains(text, "schema") || strings.Contains(text, "response_format")):
		return r.formatRemedy(byInstall)
	}
	return ""
}

// formatRemedy is the one remedy for replies not in the reply format, or a
// reply format the provider refuses.
func (r Result) formatRemedy(byInstall func(string) bool) string {
	switch {
	case r.ownAddress(), !r.StrictOutput:
		return "Use a larger model."
	case r.SchemaDialect != "openai-strict" && isOpenAI(r.Address):
		if byInstall(settings.ModelSchemaDialect) {
			return "Install with model.schemaDialect=openai-strict."
		}
		return "Set the reply format to OpenAI's strict form."
	case byInstall(settings.ModelStrictOutput):
		return "Install with model.strictOutput=false."
	}
	return "Set the reply format to Not strict."
}

// where names what was tested: its address, or Anthropic.
func (r Result) where() string {
	switch {
	case r.Address != "":
		return r.Address
	case r.ownAddress():
		return "Anthropic"
	}
	return "the model"
}

// ownAddress says the provider tested has an address of its own: Anthropic.
func (r Result) ownAddress() bool { return config.Model{Provider: r.Provider}.OwnAddress() }

func isOpenAI(address string) bool {
	u, err := url.Parse(address)
	return err == nil && strings.EqualFold(u.Hostname(), "api.openai.com")
}

// missingNames are the settings a test can lack, as the Setup page names
// them.
var missingNames = map[string]string{
	settings.ModelBaseURL: "the address",
	settings.ModelName:    "the model",
	settings.ModelAPIKey:  "the API key",
}

func missing(names []string) string {
	words := make([]string, 0, len(names))
	for _, n := range names {
		if w, ok := missingNames[n]; ok {
			words = append(words, w)
		}
	}
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// seconds is d for people: 20 s, or 1.2 s.
func seconds(d time.Duration) string {
	if d >= time.Second && d%time.Second == 0 {
		return fmt.Sprintf("%d s", d/time.Second)
	}
	return fmt.Sprintf("%.1f s", max(d.Seconds(), 0.1))
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[n:]
}

// quotedLimit is the most of a provider's text the page shows.
const quotedLimit = 240

// quoted is a provider's own text, on one line and cut short, ending as a
// sentence. Empty stays empty.
func quoted(s string) string {
	s = oneLine(s)
	if s == "" {
		return ""
	}
	if r := []rune(s); len(r) > quotedLimit {
		cut := string(r[:quotedLimit])
		if i := strings.LastIndexByte(cut, ' '); i > len(cut)/2 {
			cut = cut[:i]
		}
		return strings.TrimRight(cut, " ,;:.") + "…"
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "…") {
		return s
	}
	return s + "."
}
