package catalog_test

import (
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/scenarios"
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
		{"policy reads a check the profile doesn't run", func(c *config.Config) {
			c.Profiles[0].Checks = []string{"rollout_recent"}
			c.Profiles[0].Policy.Rules = []config.PolicyRule{{Name: "r", When: `predicates.gitops_managed != "false"`, Decision: config.DecisionDeny}}
		}, `profile p: rule r reads the check "gitops_managed", which the profile doesn't run`},
		{"policy reads a check that doesn't exist", func(c *config.Config) {
			c.Profiles[0].Policy.Rules = []config.PolicyRule{{Name: "r", When: `predicates["gitops_manged"] == "true"`, Decision: config.DecisionDeny}}
		}, `profile p: rule r reads the check "gitops_manged", which doesn't exist`},
		{"a fix justified by a check the profile doesn't run", func(c *config.Config) {
			c.Profiles[0].Checks = []string{"rollout_recent"}
			c.Profiles[0].JustifiedBy = map[string][]string{"deployment.rollback-to-revision": {"failure_onset_after_rollout"}}
		}, `profile p: justifiedBy deployment.rollback-to-revision names the check "failure_onset_after_rollout", which the profile doesn't run`},
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

const (
	packYAML = `
name: crashes
description: Pods crash
rootCauses: [{name: application_bug, description: the code fails}]
profile:
  priority: 10
  match: {alertNames: [KubePodCrashLooping], targetKinds: [Deployment.apps]}
  evidence: {allowed: [pod_states]}
  checks: [has_pods]
  policy:
    rules: [{name: r, when: 'predicates.has_pods == "true"', decision: require_approval}]
`
	checksYAML = `
- name: has_pods
  version: "1"
  description: The workload runs pods
  reads: [pod_states]
  answers:
    - when: size(pod_states.pods) > 0
      answer: true
      reason: pods
    - answer: false
      reason: no pods
`
)

// packs is a folder of scenario packs: crashes, from the YAML above as edit
// changes it, and the shipped rollout-regression.
func packs(t *testing.T, edit func(fstest.MapFS)) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{
		"crashes/scenario.yaml": {Data: []byte(packYAML)},
		"crashes/checks.yaml":   {Data: []byte(checksYAML)},
	}
	for _, f := range []string{"scenario.yaml", "checks.yaml"} {
		data, err := fs.ReadFile(scenarios.FS, "rollout-regression/"+f)
		if err != nil {
			t.Fatal(err)
		}
		fsys["rollout-regression/"+f] = &fstest.MapFile{Data: data}
	}
	edit(fsys)
	return fsys
}

func replace(fsys fstest.MapFS, name, from, to string) {
	fsys[name] = &fstest.MapFile{Data: []byte(strings.Replace(string(fsys[name].Data), from, to, 1))}
}

// A scenario pack's checks and profile join the catalog; a broken pack stops
// it, named in every problem.
func TestBuild_LoadsPacks(t *testing.T) {
	tests := []struct {
		name string
		edit func(fstest.MapFS)
		want string
	}{
		{"valid", func(fstest.MapFS) {}, ""},
		{"no scenario.yaml", func(f fstest.MapFS) { delete(f, "crashes/scenario.yaml") }, "scenario pack crashes: no scenario.yaml"},
		{
			"name not the folder's", func(f fstest.MapFS) { replace(f, "crashes/scenario.yaml", "name: crashes", "name: crash") },
			`scenario pack crashes: scenario.yaml: name "crash" must be the folder's, "crashes"`,
		},
		{
			"root cause with no description", func(f fstest.MapFS) {
				replace(f, "crashes/scenario.yaml", "description: the code fails", "description: ''")
			},
			"scenario pack crashes: scenario.yaml: rootCauses[0]: application_bug needs a description",
		},
		{
			"check that doesn't compile", func(f fstest.MapFS) {
				replace(f, "crashes/checks.yaml", "size(pod_states.pods) > 0", "size(pod_state.pods) > 0")
			},
			"scenario pack crashes: check has_pods: answer 1: when: ERROR: <input>:1:6: undeclared reference to 'pod_state'",
		},
		{
			"check Themis has", func(f fstest.MapFS) { replace(f, "crashes/checks.yaml", "has_pods", "gitops_managed") },
			"scenario pack crashes: check gitops_managed: already defined by Themis",
		},
		{
			"profile the catalog can't run", func(f fstest.MapFS) { replace(f, "crashes/scenario.yaml", "[pod_states]", "[pod_state]") },
			`scenario pack crashes: profile: unknown evidence template "pod_state"`,
		},
		{
			"priority another pack has", func(f fstest.MapFS) { replace(f, "crashes/scenario.yaml", "priority: 10", "priority: 100") },
			"scenario pack rollout-regression: profile: priority 100 is already used by the pack crashes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, err := catalog.Build(nil, nil, packs(t, tt.edit), nil)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := cat.Checks.Get("has_pods"); !ok || len(cat.Profiles) != 2 || cat.Profiles[0].Name != "crashes" {
				t.Errorf("checks %d, profiles %+v: want the pack's check and profile", len(cat.Checks), cat.Profiles)
			}
			// A profile of the install's own that no pack has may name every
			// pack's root causes, each once.
			if own := cat.RootCauses.For("own"); len(own) != 12 || own[0].Description != "the code fails" {
				t.Errorf("root causes for a profile of one's own = %+v, want crashes' then rollout-regression's others", own)
			}
			// An incident matching both packs may name either's, each once,
			// the first pack's first.
			if both := cat.RootCauses.For("crashes", "rollout-regression"); !reflect.DeepEqual(both, cat.RootCauses.For("own")) {
				t.Errorf("root causes for both packs = %+v, want crashes' then rollout-regression's others", both)
			}
		})
	}
}

// A disabled pack brings neither its profile nor its checks, while Themis's
// own checks stay for every other pack; disabling a pack Themis doesn't have
// stops it.
func TestBuild_DisablesPacks(t *testing.T) {
	cat, err := catalog.Build(nil, nil, packs(t, func(fstest.MapFS) {}), []string{"rollout-regression"})
	if err != nil {
		t.Fatal(err)
	}
	var checks []string
	for _, c := range cat.Checks {
		checks = append(checks, c.Name)
	}
	if len(cat.Profiles) != 1 || cat.Profiles[0].Name != "crashes" || !slices.Equal(checks, []string{"rollout_recent", "gitops_managed", "evidence_has_instructions", "has_pods"}) {
		t.Errorf("profiles %+v, checks %v: want only the crashes pack's, and Themis's own", cat.Profiles, checks)
	}
	_, err = catalog.Build(nil, nil, packs(t, func(fstest.MapFS) {}), []string{"rollout-regresion"})
	if want := `scenarios.disabled: no scenario pack is named "rollout-regresion"`; errText(err) != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
