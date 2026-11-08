package modeltest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/server/modeltest"
)

// What the Setup page says after a test: the outcome first, then the one
// remedy that applies, a Helm value for what Helm set.
func TestResult_Text(t *testing.T) {
	const openAI = "https://api.openai.com/v1"
	anthropic := modeltest.Result{Provider: "anthropic", Model: "claude-haiku-5-5"}
	openai := modeltest.Result{Provider: "openai", Address: openAI, Model: "gpt-4.2", StrictOutput: true, SchemaDialect: "as-is"}
	ollama := modeltest.Result{Provider: "openai", Address: "http://ollama:11434/v1", Model: "qwen3:8b", StrictOutput: true, SchemaDialect: "as-is"}
	with := func(r modeltest.Result, change func(*modeltest.Result)) modeltest.Result {
		change(&r)
		return r
	}
	none := func(string) bool { return false }
	helmDialect := func(name string) bool {
		return name == settings.ModelSchemaDialect || name == settings.ModelStrictOutput
	}
	for _, c := range []struct {
		result    modeltest.Result
		byInstall func(string) bool
		want      string
	}{
		{with(anthropic, func(r *modeltest.Result) {
			r.Outcome, r.AnsweredBy, r.Latency = modeltest.Connected, "claude-haiku-5-5", 1234*time.Millisecond
		}), none, "Connected. claude-haiku-5-5 answered in 1.2 s in the reply format."},
		{with(anthropic, func(r *modeltest.Result) { r.Outcome, r.Status = modeltest.Refused, 401 }), none, "Failed. Anthropic refused the key (401)."},
		{with(anthropic, func(r *modeltest.Result) { r.Outcome, r.Status = modeltest.Refused, 404 }), none, "Failed. Anthropic has no model named claude-haiku-5-5 (404)."},
		{with(openai, func(r *modeltest.Result) {
			r.Outcome, r.Status, r.Detail = modeltest.Refused, 404, "The model `gpt-4.2` does not exist"
		}), none, "Failed. No model named gpt-4.2 at https://api.openai.com/v1 (404)."},
		{
			with(ollama, func(r *modeltest.Result) {
				r.Outcome, r.Status, r.Detail = modeltest.Refused, 404, "404 page not found"
			}), none,
			"Failed. No chat completions at http://ollama:11434/v1 (404). Check the address, such as https://api.openai.com/v1.",
		},
		{
			with(ollama, func(r *modeltest.Result) { r.Outcome, r.Latency = modeltest.NoAnswer, 20*time.Second }), none,
			"Failed. No answer from http://ollama:11434/v1 within 20 s.",
		},
		{
			with(ollama, func(r *modeltest.Result) { r.Outcome, r.Detail = modeltest.Unreachable, "Connection refused" }), none,
			"Failed. Can't connect to http://ollama:11434/v1 (connection refused). Check the address, and that investigator.egressCIDR and investigator.egressPorts allow it.",
		},
		{
			with(anthropic, func(r *modeltest.Result) { r.Outcome = modeltest.Unreachable }), none,
			"Failed. Can't connect to Anthropic. Check that investigator.egressCIDR allows Anthropic's API.",
		},
		// OpenAI refuses the schema in strict mode unless it is in its own form.
		{with(openai, func(r *modeltest.Result) {
			r.Outcome, r.Status, r.Detail = modeltest.Refused, 400, "Invalid schema for response_format 'reply': 'required' must include every key."
		}), none, "Failed. https://api.openai.com/v1 refused the request (400): Invalid schema for response_format 'reply': 'required' must include every key. Set the reply format to OpenAI's strict form."},
		{
			with(openai, func(r *modeltest.Result) { r.Outcome = modeltest.NotInFormat }), helmDialect,
			"Failed. The model answered, but not in the reply format. Install with investigator.model.schemaDialect=openai-strict.",
		},
		{
			with(ollama, func(r *modeltest.Result) { r.Outcome = modeltest.NotInFormat }), none,
			"Failed. The model answered, but not in the reply format. Set the reply format to Not strict.",
		},
		{
			with(ollama, func(r *modeltest.Result) { r.Outcome, r.StrictOutput = modeltest.NotInFormat, false }), none,
			"Failed. The model answered, but not in the reply format. Use a larger model.",
		},
		{with(openai, func(r *modeltest.Result) {
			r.Outcome, r.Status, r.Detail = modeltest.Refused, 400, "Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."
		}), none, "Failed. https://api.openai.com/v1 refused the request (400): Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead. Install with investigator.model.outputLimitField=max_completion_tokens."},
		{
			with(openai, func(r *modeltest.Result) {
				r.Outcome, r.Status, r.Detail = modeltest.Refused, 429, "You exceeded your current quota"
			}), none,
			"Failed. https://api.openai.com/v1 refused the request (429): You exceeded your current quota.",
		},
		{
			with(ollama, func(r *modeltest.Result) { r.Outcome, r.OutputLimit = modeltest.CutOff, 512 }), none,
			"Failed. The reply was cut off at 512 tokens. Raise investigator.model.maxOutputTokens for a model that reasons.",
		},
		{modeltest.Result{Outcome: modeltest.NoInvestigator}, none, "No investigator is connected. Themis can't reach the model without it."},
		{
			modeltest.Result{Outcome: modeltest.Incomplete, Missing: []string{settings.ModelBaseURL, settings.ModelName}},
			none,
			"Not tested. Enter the address and the model first.",
		},
		{
			modeltest.Result{Outcome: modeltest.NoReport, Latency: 25 * time.Second},
			none,
			"Failed. The investigator took the test and reported nothing within 25 s. Read the themis-intelligence pod's logs.",
		},
	} {
		if got := c.result.Text(c.byInstall); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.result.Outcome, got, c.want)
		}
	}
}

// A provider's long text is cut short, on one line.
func TestResult_TextCutsTheProvidersText(t *testing.T) {
	r := modeltest.Result{Outcome: modeltest.Refused, Provider: "openai", Address: "http://m/v1", Status: 500, Detail: strings.Repeat("overloaded\n", 60)}
	got := r.Text(func(string) bool { return false })
	if len(got) > 320 || strings.Contains(got, "\n") || !strings.HasSuffix(got, "…") {
		t.Errorf("Text = %q", got)
	}
}
