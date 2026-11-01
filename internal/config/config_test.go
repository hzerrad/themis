package config_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
)

const validYAML = `
enrolledNamespaces: [shop, payments]
intake:
  bearerTokenFile: /var/run/secrets/themis/intake-token
investigators:
  bearerTokenFile: /var/run/secrets/themis/investigator-token
api:
  bearerTokenFile: /var/run/secrets/themis/admin-token
profiles:
  - name: rollout-regression
    priority: 100
    match:
      alertNames: [KubePodCrashLooping, KubePodOOMKilled]
      targetKinds: [Deployment.apps]
    evidence:
      initial: [rollout_history, pod_states]
      allowed: [rollout_history, pod_states, container_logs_tail]
    capabilities: [deployment.rollback-to-revision]
    investigation:
      maxSteps: 4
      timeout: 2m
  - name: critical-crashloop
    priority: 200
    match:
      alertNames: [KubePodCrashLooping]
      labels:
        severity: critical
      targetKinds: [Deployment.apps]
    evidence:
      allowed: [pod_states]
`

func mustParse(t *testing.T, doc string) config.Config {
	t.Helper()
	c, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func TestParse_Valid(t *testing.T) {
	c := mustParse(t, validYAML)

	if len(c.EnrolledNamespaces) != 2 || len(c.Profiles) != 2 {
		t.Fatalf("parsed %d namespaces and %d profiles, want 2 and 2", len(c.EnrolledNamespaces), len(c.Profiles))
	}
	if got := c.Profiles[1].Match.Labels["severity"]; got != "critical" {
		t.Errorf("profiles[1].match.labels.severity = %q, want critical", got)
	}
}

func TestParse_ExampleFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "config", "themis.example.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if _, err := config.Parse(data); err != nil {
		t.Fatalf("the shipped example config is invalid: %v", err)
	}
}

// The configuration `make run` uses must stay valid as the schema evolves.
func TestParse_DevFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "dev", "themis.yaml"))
	if err != nil {
		t.Fatalf("read dev config: %v", err)
	}
	if _, err := config.Parse(data); err != nil {
		t.Fatalf("scripts/dev/themis.yaml is invalid: %v", err)
	}
}

func TestParse_UnknownFieldIsAnError(t *testing.T) {
	doc := strings.Replace(validYAML, "priority: 100", "priority: 100\n    prority: 5", 1)

	_, err := config.Parse([]byte(doc))

	if err == nil || !strings.Contains(err.Error(), "prority") {
		t.Fatalf("err = %v, want an error naming the unknown field", err)
	}
}

func TestParse_DuplicateKeyIsAnError(t *testing.T) {
	doc := strings.Replace(validYAML, "intake:", "intake:\n  bearerTokenFile: /a", 1)

	if _, err := config.Parse([]byte(doc)); err == nil {
		t.Fatal("err = nil, want an error for the duplicate bearerTokenFile key")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestValidate_Errors(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(c *config.Config)
		wantPath string
	}{
		{"no namespaces", func(c *config.Config) { c.EnrolledNamespaces = nil }, "enrolledNamespaces"},
		{"invalid namespace", func(c *config.Config) { c.EnrolledNamespaces[0] = "Shop_1" }, "enrolledNamespaces[0]"},
		{"duplicate namespace", func(c *config.Config) { c.EnrolledNamespaces[1] = "shop" }, "enrolledNamespaces[1]"},
		{"no token file", func(c *config.Config) { c.Intake.BearerTokenFile = "" }, "intake.bearerTokenFile"},
		{"webhook not http", func(c *config.Config) { c.Notifications.Webhook = &config.Webhook{URL: "file:///etc/passwd"} }, "notifications.webhook.url"},
		{"webhook without url", func(c *config.Config) { c.Notifications.Webhook = &config.Webhook{} }, "notifications.webhook"},
		{"bad external URL", func(c *config.Config) { c.Notifications.ExternalURL = "themis.local" }, "notifications.externalURL"},
		{"executor without address", func(c *config.Config) { c.Executor = &config.Executor{BearerTokenFile: "/t"} }, "executor.address"},
		{"no admin token file", func(c *config.Config) { c.API.BearerTokenFile = "" }, "api.bearerTokenFile"},
		{"no investigator token file", func(c *config.Config) { c.Investigators.BearerTokenFile = "" }, "investigators.bearerTokenFile"},
		{"no profiles", func(c *config.Config) { c.Profiles = nil }, "profiles"},
		{"invalid profile name", func(c *config.Config) { c.Profiles[0].Name = "Rollout Regression" }, "profiles[0].name"},
		{"duplicate profile name", func(c *config.Config) { c.Profiles[1].Name = c.Profiles[0].Name }, "profiles[1].name"},
		{"duplicate priority", func(c *config.Config) { c.Profiles[1].Priority = c.Profiles[0].Priority }, "profiles[1].priority"},
		{"missing priority", func(c *config.Config) { c.Profiles[0].Priority = 0 }, "profiles[0].priority"},
		{"negative priority", func(c *config.Config) { c.Profiles[1].Priority = -5 }, "profiles[1].priority"},
		{"no alert names", func(c *config.Config) { c.Profiles[0].Match.AlertNames = nil }, "profiles[0].match.alertNames"},
		{"empty alert name", func(c *config.Config) { c.Profiles[0].Match.AlertNames[1] = "" }, "profiles[0].match.alertNames[1]"},
		{"invalid label key", func(c *config.Config) { c.Profiles[1].Match.Labels = map[string]string{"bad key": "x"} }, "profiles[1].match.labels"},
		{"invalid label value", func(c *config.Config) { c.Profiles[1].Match.Labels = map[string]string{"severity": "not ok!"} }, "profiles[1].match.labels"},
		{"no target kinds", func(c *config.Config) { c.Profiles[0].Match.TargetKinds = nil }, "profiles[0].match.targetKinds"},
		{"lowercase kind", func(c *config.Config) { c.Profiles[0].Match.TargetKinds = []string{"deployment.apps"} }, "profiles[0].match.targetKinds[0]"},
		{"invalid group", func(c *config.Config) { c.Profiles[0].Match.TargetKinds = []string{"Deployment.Apps"} }, "profiles[0].match.targetKinds[0]"},
		{"kind with slash", func(c *config.Config) { c.Profiles[0].Match.TargetKinds = []string{"apps/Deployment"} }, "profiles[0].match.targetKinds[0]"},
		{"no allowed evidence", func(c *config.Config) { c.Profiles[1].Evidence.Allowed = nil }, "profiles[1].evidence.allowed"},
		{"duplicate allowed evidence", func(c *config.Config) { c.Profiles[1].Evidence.Allowed = []string{"pod_states", "pod_states"} }, "profiles[1].evidence.allowed[1]"},
		{"initial not allowed", func(c *config.Config) { c.Profiles[0].Evidence.Initial = []string{"target_events"} }, "profiles[0].evidence.initial[0]"},
		{"unknown capability", func(c *config.Config) { c.Profiles[0].Capabilities = []string{"deployment.delete"} }, "profiles[0].capabilities[0]"},
		{"negative max steps", func(c *config.Config) { c.Profiles[0].Investigation.MaxSteps = -1 }, "profiles[0].investigation.maxSteps"},
		{"unknown mode", func(c *config.Config) { c.Profiles[0].Policy.Mode = "autonomous" }, "profiles[0].policy.mode"},
		{"rule without condition", func(c *config.Config) {
			c.Profiles[0].Policy.Rules = []config.PolicyRule{{Name: "r", Decision: config.DecisionDeny}}
		}, "profiles[0].policy.rules[0].when"},
		{"unknown decision", func(c *config.Config) {
			c.Profiles[0].Policy.Rules = []config.PolicyRule{{Name: "r", When: "true", Decision: "maybe"}}
		}, "profiles[0].policy.rules[0].decision"},
		{"stability beyond window", func(c *config.Config) {
			c.Profiles[0].Verification = config.Verification{Window: config.Duration{Duration: time.Minute}, Stability: config.Duration{Duration: time.Hour}}
		}, "profiles[0].verification"},
		{"negative timeout", func(c *config.Config) { c.Profiles[0].Investigation.Timeout.Duration = -time.Second }, "profiles[0].investigation.timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mustParse(t, validYAML)
			tt.mutate(&c)

			err := c.Validate()

			if err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantPath+":") {
				t.Errorf("error does not name %s:\n%v", tt.wantPath, err)
			}
		})
	}
}

func TestValidate_ReportsEveryError(t *testing.T) {
	c := mustParse(t, validYAML)
	c.EnrolledNamespaces = nil
	c.Intake.BearerTokenFile = ""
	c.Profiles[0].Match.TargetKinds = nil

	err := c.Validate()

	for _, path := range []string{"enrolledNamespaces:", "intake.bearerTokenFile:", "profiles[0].match.targetKinds:"} {
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("error does not mention %s\n%v", path, err)
		}
	}
}

func TestValidate_AcceptsAnyWellFormedKind(t *testing.T) {
	c := mustParse(t, validYAML)
	c.Profiles[0].Match.TargetKinds = []string{
		"Deployment.apps", "StatefulSet.apps", "Job.batch", "PersistentVolumeClaim", "Rollout.argoproj.io",
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestIsEnrolled(t *testing.T) {
	c := mustParse(t, validYAML)

	if !c.IsEnrolled("shop") || !c.IsEnrolled("payments") {
		t.Error("an enrolled namespace was reported as not enrolled")
	}
	if c.IsEnrolled("kube-system") || c.IsEnrolled("") {
		t.Error("a namespace that is not enrolled was reported as enrolled")
	}
}

func TestTargetKind(t *testing.T) {
	if got := config.TargetKind(incident.TargetRef{Group: "apps", Kind: "Deployment"}); got != "Deployment.apps" {
		t.Errorf("TargetKind(apps/Deployment) = %q, want Deployment.apps", got)
	}
	if got := config.TargetKind(incident.TargetRef{Group: "", Kind: "Pod"}); got != "Pod" {
		t.Errorf("TargetKind(core/Pod) = %q, want Pod", got)
	}
}

var deployment = incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "u1"}

func TestMatchProfile(t *testing.T) {
	c := mustParse(t, validYAML)

	tests := []struct {
		name      string
		alertName string
		labels    map[string]string
		target    incident.TargetRef
		want      string // profile name; "" means no match
	}{
		{
			name:      "matches by alert name and kind",
			alertName: "KubePodOOMKilled",
			labels:    map[string]string{"namespace": "shop"},
			target:    deployment,
			want:      "rollout-regression",
		},
		{
			name:      "higher priority wins when both match",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"namespace": "shop", "severity": "critical"},
			target:    deployment,
			want:      "critical-crashloop",
		},
		{
			name:      "label selector must match",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"namespace": "shop", "severity": "warning"},
			target:    deployment,
			want:      "rollout-regression",
		},
		{
			name:      "missing selector label does not match",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"namespace": "shop"},
			target:    deployment,
			want:      "rollout-regression",
		},
		{
			name:      "unknown alert name",
			alertName: "KubeNodeNotReady",
			labels:    map[string]string{"severity": "critical"},
			target:    deployment,
			want:      "",
		},
		{
			name:      "kind does not match",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"severity": "critical"},
			target:    incident.TargetRef{Group: "apps", Kind: "StatefulSet", Namespace: "shop", Name: "db"},
			want:      "",
		},
		{
			name:      "same kind in another API group does not match",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"severity": "critical"},
			target:    incident.TargetRef{Group: "example.com", Kind: "Deployment", Namespace: "shop", Name: "x"},
			want:      "",
		},
		{
			name:      "nil labels",
			alertName: "KubePodOOMKilled",
			labels:    nil,
			target:    deployment,
			want:      "rollout-regression",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := c.MatchProfile(tt.alertName, tt.labels, tt.target)

			switch {
			case tt.want == "" && ok:
				t.Errorf("matched %q, want no match", p.Name)
			case tt.want != "" && !ok:
				t.Errorf("no match, want %q", tt.want)
			case tt.want != "" && p.Name != tt.want:
				t.Errorf("matched %q, want %q", p.Name, tt.want)
			}
		})
	}
}

// Priority, not position in the file, decides the winner.
func TestMatchProfile_PriorityNotFileOrder(t *testing.T) {
	c := mustParse(t, validYAML)
	c.Profiles[0], c.Profiles[1] = c.Profiles[1], c.Profiles[0]

	p, ok := c.MatchProfile("KubePodCrashLooping", map[string]string{"severity": "critical"}, deployment)

	if !ok || p.Name != "critical-crashloop" {
		t.Fatalf("matched %q (ok=%v), want critical-crashloop", p.Name, ok)
	}
}

// MatchProfile does not rely on Validate: a matching profile is returned even
// when its priority is one Validate would reject.
func TestMatchProfile_UnvalidatedPriority(t *testing.T) {
	c := mustParse(t, validYAML)
	c.Profiles = c.Profiles[:1]
	c.Profiles[0].Priority = 0

	p, ok := c.MatchProfile("KubePodOOMKilled", nil, deployment)

	if !ok || p.Name != "rollout-regression" {
		t.Fatalf("matched %q (ok=%v), want rollout-regression", p.Name, ok)
	}
}

func TestParse_Investigation(t *testing.T) {
	c := mustParse(t, validYAML)
	got := c.Profiles[0].InvestigationBudget()
	if got.MaxSteps != 4 || got.Timeout.Duration != 2*time.Minute {
		t.Errorf("budget = %+v, want 4 steps in 2m", got)
	}
	if got.MaxEvidenceRequests != config.DefaultMaxEvidenceRequests {
		t.Errorf("MaxEvidenceRequests = %d, want the default", got.MaxEvidenceRequests)
	}
	if d := c.Profiles[1].InvestigationBudget(); d.MaxSteps != config.DefaultMaxSteps || d.Timeout.Duration != config.DefaultInvestigationTimeout {
		t.Errorf("defaults = %+v", d)
	}
}

func TestParse_InvalidDuration(t *testing.T) {
	doc := strings.Replace(validYAML, "timeout: 2m", "timeout: 2 minutes", 1)
	if _, err := config.Parse([]byte(doc)); err == nil {
		t.Error("Parse accepted an invalid duration")
	}
}

func TestProfile_AllowsTemplate(t *testing.T) {
	p := mustParse(t, validYAML).Profiles[0]
	if !p.AllowsTemplate("container_logs_tail") || p.AllowsTemplate("target_events") {
		t.Error("AllowsTemplate does not follow evidence.allowed")
	}
}

func TestProfile_PolicyAndVerificationDefaults(t *testing.T) {
	p := mustParse(t, validYAML).Profiles[1]
	if pol := p.EffectivePolicy(); pol.Mode != config.ModeGuarded || pol.ApprovalTimeout.Duration != config.DefaultApprovalTimeout {
		t.Errorf("policy = %+v", pol)
	}
	if v := p.EffectiveVerification(); v.Window.Duration != config.DefaultVerifyWindow || v.Stability.Duration != config.DefaultVerifyStability {
		t.Errorf("verification = %+v", v)
	}
}

func TestValidate_ProfilesComeFromOnePlace(t *testing.T) {
	c := mustParse(t, validYAML)
	c.Resources = &config.Resources{Namespace: "themis"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "IncidentProfile") {
		t.Errorf("both sources: %v", err)
	}
	c.Profiles = nil
	if err := c.Validate(); err != nil {
		t.Errorf("resources alone: %v", err)
	}
	c.Resources = nil
	if err := c.Validate(); err == nil {
		t.Error("no profiles and no resources passed")
	}
}

func TestMatchProfile_ScopeLimitsAProfileToItsNamespace(t *testing.T) {
	c := mustParse(t, validYAML)
	p := c.Profiles[0]
	p.Scope = "team"
	c.Profiles = []config.Profile{p}
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "other"}
	if _, ok := c.MatchProfile(p.Match.AlertNames[0], p.Match.Labels, target); ok {
		t.Error("a scoped profile matched outside its namespace")
	}
	target.Namespace = "team"
	if _, ok := c.MatchProfile(p.Match.AlertNames[0], p.Match.Labels, target); !ok {
		t.Error("a scoped profile didn't match in its namespace")
	}
}
