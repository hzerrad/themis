package config_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
    justifiedBy:
      deployment.rollback-to-revision: [failure_onset_after_rollout]
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

// The shipped example and the configuration `make run` uses must stay valid as
// the schema evolves.
func TestParse_ShippedFiles(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "deploy", "config", "themis.example.yaml"),
		filepath.Join("..", "..", "scripts", "dev", "themis.yaml"),
	} {
		data, err := os.ReadFile(path) //nolint:gosec // a path chosen by the test
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := config.Parse(data); err != nil {
			t.Errorf("%s is invalid: %v", path, err)
		}
	}
}

func TestParse_Refuses(t *testing.T) {
	tests := []struct {
		name, from, to string
		mention        string // a word the error names; "" for any error
	}{
		{"unknown field", "priority: 100", "priority: 100\n    prority: 5", "prority"},
		{"duplicate key", "intake:", "intake:\n  bearerTokenFile: /a", ""},
		{"invalid duration", "timeout: 2m", "timeout: 2 minutes", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(strings.Replace(validYAML, tt.from, tt.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("err = %v, want an error naming %q", err, tt.mention)
			}
		})
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
		{"unknown model provider", func(c *config.Config) { c.Model.Provider = "gemini" }, "model.provider"},
		{"prometheus not http", func(c *config.Config) { c.Metrics.Prometheus = &config.Prometheus{Address: "prometheus:9090"} }, "metrics.prometheus.address"},
		{"client certificate without key", func(c *config.Config) {
			c.Metrics.Prometheus = &config.Prometheus{Address: "https://prometheus", TLS: config.TLS{CertFile: "/c"}}
		}, "metrics.prometheus.tls"},
		{"unknown schema dialect", func(c *config.Config) { c.Model.SchemaDialect = "strict" }, "model.schemaDialect"},
		{"no admin token file", func(c *config.Config) { c.API.BearerTokenFile = "" }, "api.bearerTokenFile"},
		{"initial admin without its password", func(c *config.Config) { c.API.InitialAdmin = &config.InitialAdmin{} }, "api.initialAdmin.passwordFile"},
		{"admin account without a username", func(c *config.Config) { c.API.AdminAccount = &config.AdminAccount{PasswordFile: "/p"} }, "api.adminAccount.username"},
		{"admin account without a password", func(c *config.Config) { c.API.AdminAccount = &config.AdminAccount{Username: "root"} }, "api.adminAccount"},
		{"admin account and initial admin", func(c *config.Config) {
			c.API.InitialAdmin = &config.InitialAdmin{PasswordFile: "/i"}
			c.API.AdminAccount = &config.AdminAccount{Username: "root", PasswordHashFile: "/h"}
		}, "api.adminAccount"},
		{"no investigator token file", func(c *config.Config) { c.Investigators.BearerTokenFile = "" }, "investigators.bearerTokenFile"},
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
		{"a fix nothing justifies", func(c *config.Config) { c.Profiles[0].JustifiedBy = nil }, "profiles[0].justifiedBy"},
		{"justifies a fix it doesn't offer", func(c *config.Config) {
			c.Profiles[1].JustifiedBy = map[string][]string{"deployment.set-probes": {"probe_kills"}}
		}, "profiles[1].justifiedBy"},
		{"guidance too long", func(c *config.Config) { c.Profiles[0].Guidance = strings.Repeat("é", config.MaxGuidance+1) }, "profiles[0].guidance"},
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

var deployment = incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "u1"}

// An alert matches every profile whose alert names, target kinds and labels
// it meets, highest priority first, whatever their order in the file.
func TestMatch(t *testing.T) {
	c := mustParse(t, validYAML)

	tests := []struct {
		name      string
		alertName string
		labels    map[string]string
		target    incident.TargetRef
		want      []string // profile names, highest priority first
	}{
		{
			name:      "matches by alert name and kind",
			alertName: "KubePodOOMKilled",
			labels:    map[string]string{"namespace": "shop"},
			target:    deployment,
			want:      []string{"rollout-regression"},
		},
		{
			name:      "every profile that matches, higher priority first",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"namespace": "shop", "severity": "critical"},
			target:    deployment,
			want:      []string{"critical-crashloop", "rollout-regression"},
		},
		{
			name:      "label selector must match",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"namespace": "shop", "severity": "warning"},
			target:    deployment,
			want:      []string{"rollout-regression"},
		},
		{
			name:      "missing selector label does not match",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"namespace": "shop"},
			target:    deployment,
			want:      []string{"rollout-regression"},
		},
		{
			name:      "unknown alert name",
			alertName: "KubeNodeNotReady",
			labels:    map[string]string{"severity": "critical"},
			target:    deployment,
		},
		{
			name:      "kind does not match",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"severity": "critical"},
			target:    incident.TargetRef{Group: "apps", Kind: "StatefulSet", Namespace: "shop", Name: "db"},
		},
		{
			name:      "same kind in another API group does not match",
			alertName: "KubePodCrashLooping",
			labels:    map[string]string{"severity": "critical"},
			target:    incident.TargetRef{Group: "example.com", Kind: "Deployment", Namespace: "shop", Name: "x"},
		},
		{
			name:      "nil labels",
			alertName: "KubePodOOMKilled",
			target:    deployment,
			want:      []string{"rollout-regression"},
		},
	}
	swapped := c
	swapped.Profiles = []config.Profile{c.Profiles[1], c.Profiles[0]}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, cfg := range []config.Config{c, swapped} {
				if got := cfg.Match(tt.alertName, tt.labels, tt.target).Names(); !slices.Equal(got, tt.want) {
					t.Errorf("matched %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// An incident matching several profiles gets what each brings: every check,
// template and fix any of them has, each once, the largest budget, and each
// one's guidance, introduced by its name and its fixes. Of what decides a
// fix, the strictest wins.
func TestMatched(t *testing.T) {
	rollout := config.Profile{
		Name: "rollout", Checks: []string{"rollout_recent", "gitops_managed"},
		Evidence:     config.Evidence{Initial: []string{"pod_states"}, Allowed: []string{"pod_states", "rollout_history"}},
		Capabilities: []string{"deployment.rollback-to-revision"}, Guidance: "Roll back a bad rollout.",
		Investigation: config.Investigation{MaxSteps: 4},
		Policy:        config.Policy{ApprovalTimeout: config.Duration{Duration: time.Hour}},
		Verification:  config.Verification{Window: config.Duration{Duration: 10 * time.Minute}},
	}
	memory := config.Profile{
		Name: "memory", Checks: []string{"gitops_managed", "memory_near_limit"},
		Evidence:     config.Evidence{Initial: []string{"container_memory"}, Allowed: []string{"container_memory", "pod_states"}},
		Capabilities: []string{"deployment.set-resources"}, Guidance: "Raise the limit.\n",
		Investigation: config.Investigation{MaxEvidenceRequests: 9},
		Policy:        config.Policy{ApprovalTimeout: config.Duration{Duration: 10 * time.Minute}},
	}
	diagnoses := config.Profile{Name: "diagnoses", Checks: []string{"image_pull_failing"}}
	everything := config.Profile{Name: "everything"}

	for _, tt := range []struct {
		name string
		m    config.Matched
		want config.Matched
	}{
		{name: "one profile is itself", m: config.Matched{rollout}, want: config.Matched{rollout}},
		{
			name: "several are combined", m: config.Matched{rollout, memory, diagnoses},
			want: config.Matched{{
				Checks: []string{"rollout_recent", "gitops_managed", "memory_near_limit", "image_pull_failing"},
				Evidence: config.Evidence{
					Initial: []string{"pod_states", "container_memory"},
					Allowed: []string{"pod_states", "rollout_history", "container_memory"},
				},
				Capabilities: []string{"deployment.rollback-to-revision", "deployment.set-resources"},
				Guidance: "From the rollout profile, which offers deployment.rollback-to-revision:\nRoll back a bad rollout.\n\n" +
					"From the memory profile, which offers deployment.set-resources:\nRaise the limit.",
				Investigation: config.Investigation{MaxSteps: 8, MaxEvidenceRequests: 9, Timeout: config.Duration{Duration: 5 * time.Minute}},
				Policy:        config.Policy{ApprovalTimeout: config.Duration{Duration: 10 * time.Minute}},
				Verification:  config.Verification{Window: config.Duration{Duration: 10 * time.Minute}, Stability: config.Duration{Duration: 2 * time.Minute}},
			}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.want[0]
			if got := tt.m.Checks(); !slices.Equal(got, want.Checks) {
				t.Errorf("checks = %v, want %v", got, want.Checks)
			}
			if got := tt.m.Evidence(); !reflect.DeepEqual(got, want.Evidence) {
				t.Errorf("evidence = %+v, want %+v", got, want.Evidence)
			}
			if got := tt.m.Capabilities(); !slices.Equal(got, want.Capabilities) {
				t.Errorf("fixes = %v, want %v", got, want.Capabilities)
			}
			if got := tt.m.Guidance(); got != want.Guidance {
				t.Errorf("guidance = %q, want %q", got, want.Guidance)
			}
			if got, b := tt.m.InvestigationBudget(), want.InvestigationBudget(); got != b {
				t.Errorf("budget = %+v, want %+v", got, b)
			}
			if got := tt.m.ApprovalTimeout(); got != want.EffectivePolicy().ApprovalTimeout.Duration {
				t.Errorf("approval timeout = %s, want the shortest", got)
			}
			if got := tt.m.Verification(); got != want.EffectiveVerification() {
				t.Errorf("verification = %+v, want the longest", got)
			}
		})
	}
	// A profile naming no check runs every check, and so do they all.
	if got := (config.Matched{rollout, everything}).Checks(); got != nil {
		t.Errorf("checks = %v, want every check", got)
	}
	if got := (config.Matched{rollout, memory, diagnoses}).Offering("deployment.set-resources").Names(); !slices.Equal(got, []string{"memory"}) {
		t.Errorf("governing the memory fix: %v, want only the profile offering it", got)
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
	// The scenario packs' profiles stand in for the file's.
	c.Resources = nil
	if err := c.Validate(); err != nil {
		t.Errorf("neither: %v", err)
	}
}

// An install's own profile replaces a scenario pack's of the same name, and
// wins a tie in priority with another.
func TestWithProfiles_TheInstallsOwnWin(t *testing.T) {
	c := mustParse(t, validYAML)
	own := c.Profiles[0]
	pack := func(name string, priority int) config.Profile {
		p := own
		p.Name, p.Priority, p.Capabilities = name, priority, []string{"from-the-pack"}
		return p
	}
	c.Profiles = []config.Profile{own}

	got := c.WithProfiles([]config.Profile{pack(own.Name, own.Priority+1), pack("other", own.Priority)})
	var names []string
	for _, p := range got.Profiles {
		names = append(names, p.Name)
	}
	if want := []string{own.Name, "other"}; !slices.Equal(names, want) {
		t.Fatalf("profiles = %v, want %v", names, want)
	}
	if !reflect.DeepEqual(got.Profiles[0], own) {
		t.Errorf("the pack's profile replaced the install's: %+v", got.Profiles[0])
	}
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop"}
	if p := got.Match(own.Match.AlertNames[0], own.Match.Labels, target).Primary(); p.Name != own.Name {
		t.Errorf("a tie went to %s, want the install's %s", p.Name, own.Name)
	}
}

func TestMatch_ScopeLimitsAProfileToItsNamespace(t *testing.T) {
	c := mustParse(t, validYAML)
	p := c.Profiles[0]
	p.Scope = "team"
	c.Profiles = []config.Profile{p}
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "other"}
	if m := c.Match(p.Match.AlertNames[0], p.Match.Labels, target); len(m) > 0 {
		t.Error("a scoped profile matched outside its namespace")
	}
	target.Namespace = "team"
	if m := c.Match(p.Match.AlertNames[0], p.Match.Labels, target); len(m) == 0 {
		t.Error("a scoped profile didn't match in its namespace")
	}
}

func TestExternalProposals_Validate(t *testing.T) {
	good := config.ProposalSource{ServiceAccount: "agents/triage", Namespaces: []string{"shop"}, Capabilities: []string{"deployment.rollback-to-revision"}}
	tests := []struct {
		name string
		edit func(*config.ProposalSource)
		want string
	}{
		{"valid", func(*config.ProposalSource) {}, ""},
		{"no namespace in the account", func(s *config.ProposalSource) { s.ServiceAccount = "triage" }, "namespace/name"},
		{"no namespaces", func(s *config.ProposalSource) { s.Namespaces = nil }, "at least one namespace"},
		{"no capabilities", func(s *config.ProposalSource) { s.Capabilities = nil }, "at least one capability"},
		{"negative rate", func(s *config.ProposalSource) { s.PerMinute = -1 }, "positive"},
		{"unknown ceiling", func(s *config.ProposalSource) { s.Ceiling = "top-secret" }, "internal or sensitive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := good
			tt.edit(&s)
			err := config.ExternalProposals{Sources: []config.ProposalSource{s}}.Validate()
			if tt.want == "" && err != nil {
				t.Errorf("err = %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if err := (config.ExternalProposals{Sources: []config.ProposalSource{good, good}}).Validate(); err == nil {
		t.Error("a duplicate source passed")
	}
}

func TestExternalProposals_EffectiveWindow(t *testing.T) {
	one := []config.ProposalSource{{ServiceAccount: "a/b"}}
	if got := (config.ExternalProposals{}).EffectiveWindow(); got != 0 {
		t.Errorf("no sources: %s, want none, so nothing changes for installs that don't use this", got)
	}
	if got := (config.ExternalProposals{Sources: one}).EffectiveWindow(); got != config.DefaultProposalWindow {
		t.Errorf("sources: %s", got)
	}
	if got := (config.ExternalProposals{Window: config.Duration{Duration: time.Hour}}).EffectiveWindow(); got != time.Hour {
		t.Errorf("explicit: %s", got)
	}
}

func TestOIDC_ScopesAlwaysIncludeOpenID(t *testing.T) {
	for _, tc := range []struct {
		scopes, want []string
	}{
		{nil, []string{"openid", "email", "profile"}},
		{[]string{"email", "groups"}, []string{"openid", "email", "groups"}},
		{[]string{"groups", "openid"}, []string{"groups", "openid"}},
	} {
		if got := (config.OIDC{Scopes: tc.scopes}).WithDefaults().Scopes; !slices.Equal(got, tc.want) {
			t.Errorf("scopes %v = %v, want %v", tc.scopes, got, tc.want)
		}
	}
}
