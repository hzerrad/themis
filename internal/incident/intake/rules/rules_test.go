package rules_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/incident/intake/rules"
	"github.com/hzerrad/themis/internal/platform/fetch"
	"github.com/hzerrad/themis/scenarios"
)

// Every alert a scenario pack answers has a rule Themis ships, so the Setup
// page can offer one for each missing.
func TestShipped_AnswersEveryPackAlert(t *testing.T) {
	shipped, err := rules.Shipped()
	if err != nil {
		t.Fatal(err)
	}
	packs, err := fs.Glob(scenarios.FS, "*/scenario.yaml")
	if err != nil || len(packs) == 0 {
		t.Fatalf("packs: %v, %v", packs, err)
	}
	for _, path := range packs {
		b, err := fs.ReadFile(scenarios.FS, path)
		if err != nil {
			t.Fatal(err)
		}
		var pack struct {
			Profile struct {
				Match struct {
					AlertNames []string `json:"alertNames"`
				} `json:"match"`
			} `json:"profile"`
		}
		if err := yaml.Unmarshal(b, &pack); err != nil {
			t.Fatal(err)
		}
		if _, _, none := shipped.For(pack.Profile.Match.AlertNames); len(none) > 0 {
			t.Errorf("%s answers %v, which have no rule in internal/incident/intake/rules/rules.yaml", path, none)
		}
	}
}

// The rules for the alerts named come in one group, each once, with the
// names Themis has no rule for.
func TestFor(t *testing.T) {
	shipped, err := rules.Shipped()
	if err != nil {
		t.Fatal(err)
	}
	file, standard, none := shipped.For([]string{"KubePodOOMKilled", "Custom"})
	if standard || !slices.Equal(none, []string{"Custom"}) || len(file.Groups) != 1 || file.Groups[0].Name != rules.Group ||
		len(file.Groups[0].Rules) != 1 || file.Groups[0].Rules[0].Alert != "KubePodOOMKilled" {
		t.Fatalf("For = %+v, %v, %v", file, standard, none)
	}
	if _, standard, _ := shipped.For([]string{"KubePodCrashLooping"}); !standard {
		t.Error("KubePodCrashLooping isn't marked as a rule kube-prometheus-stack has")
	}
	var back rules.RuleFile
	if err := yaml.UnmarshalStrict([]byte(file.YAML()), &back); err != nil || back.Groups[0].Rules[0].Expr != file.Groups[0].Rules[0].Expr {
		t.Errorf("the rule file doesn't read back: %v\n%s", err, file.YAML())
	}
	var resource struct {
		Kind string
		Spec rules.RuleFile
	}
	if err := yaml.Unmarshal([]byte(file.PrometheusRule("themis-alert-rules")), &resource); err != nil || resource.Kind != "PrometheusRule" || len(resource.Spec.Groups) != 1 {
		t.Errorf("PrometheusRule = %+v, %v", resource, err)
	}
	if empty, _, _ := shipped.For(nil); len(empty.Groups) != 0 {
		t.Errorf("For(nothing) = %+v", empty)
	}
}

// Check asks Prometheus for the alerting rules of the alerts answered.
func TestCheck(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"status": "success", "data": {"groups": [
			{"name": "kubernetes-apps", "rules": [
				{"name": "KubePodCrashLooping", "type": "alerting"},
				{"name": "KubePodOOMKilled", "type": "recording"}]}]}}`))
	}))
	defer srv.Close()
	p, err := rules.NewPrometheus(srv.URL, fetch.Credentials{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Check(t.Context(), []string{"KubePodOOMKilled", "KubePodCrashLooping"})
	if err != nil || got.Address != srv.URL || !slices.Equal(got.Exist, []string{"KubePodCrashLooping"}) || !slices.Equal(got.Missing, []string{"KubePodOOMKilled"}) {
		t.Errorf("Check = %+v, %v", got, err)
	}
	if !strings.Contains(query, "type=alert") || !strings.Contains(query, "rule_name%5B%5D=KubePodOOMKilled") {
		t.Errorf("asked %q, want the alerting rules of the alerts answered", query)
	}
	srv.Close()
	if _, err := p.Check(t.Context(), []string{"KubePodOOMKilled"}); fetch.KindOf(err) != fetch.Unreachable {
		t.Errorf("Check of a Prometheus that doesn't answer = %v", err)
	}
}
