package catalog_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/config"
)

// A configuration is checked against what this install can read, check and
// change: one error per problem, naming the profile it is in.
func TestCheckConfig(t *testing.T) {
	tests := []struct {
		name string
		edit func(*config.Config)
		want string
	}{
		{"valid", func(*config.Config) {}, ""},
		{"unknown template", func(c *config.Config) {
			c.Profiles[0].Evidence = config.Evidence{Initial: []string{"pod_state"}, Allowed: []string{"pod_state", "target_events"}}
		}, `profile p: unknown evidence template "pod_state"`},
		{"unknown check", func(c *config.Config) { c.Profiles[0].Checks = []string{"rollout_recent", "oom_killed"} }, `profile p: unknown check "oom_killed"`},
		{"unknown fix", func(c *config.Config) { c.Profiles[0].Capabilities = []string{"deployment.delete"} }, `profile p: unknown capability "deployment.delete"`},
		{"policy that doesn't compile", func(c *config.Config) {
			c.Profiles[0].Policy.Rules = []config.PolicyRule{{Name: "r", When: "action.capability", Decision: config.DecisionDeny}}
		}, "profile p: rule r: must be a boolean, not string"},
		{"unknown fix for an outside agent", func(c *config.Config) {
			c.ExternalProposals.Sources = []config.ProposalSource{{ServiceAccount: "agents/triage", Capabilities: []string{"delete.everything"}}}
		}, `externalProposals: sources[0].capabilities[0]: unknown capability "delete.everything"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := config.Config{Profiles: []config.Profile{{
				Name:         "p",
				Evidence:     config.Evidence{Initial: []string{"pod_states"}, Allowed: []string{"pod_states", "target_events"}},
				Capabilities: []string{"deployment.rollback-to-revision"},
				Policy:       config.Policy{Rules: []config.PolicyRule{{Name: "r", When: "true", Decision: config.DecisionDeny}}},
			}}}
			tt.edit(&c)
			cat, err := catalog.Themis(nil)
			if err != nil {
				t.Fatal(err)
			}
			err = cat.CheckConfig(c)
			if got := errText(err); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
