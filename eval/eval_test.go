package eval_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/eval"
	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/evidence"
)

var update = flag.Bool("update", false, "rewrite each case's bundle.json")

const (
	packs = "../scenarios"
	oom   = packs + "/rollout-regression/eval/rollout-oom"
)

// Every scenario pack passes the conformance suite: each case's checks give
// its right answers, with no model involved, and its committed bundle is what
// the code produces today.
func TestPacks(t *testing.T) {
	entries, err := os.ReadDir(packs)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			r, err := eval.CheckPack(t.Context(), packs, e.Name(), *update)
			if err != nil {
				t.Fatal(err)
			}
			if r.Cases == 0 {
				t.Error("no cases")
			}
		})
	}
}

// A new pack, as the scaffold makes it, fails the conformance suite with what
// is left to fill in.
func TestScaffold_FailsUntilFilledIn(t *testing.T) {
	root := t.TempDir()
	if err := eval.Scaffold(root, "my-pack"); err != nil {
		t.Fatal(err)
	}
	_, err := eval.CheckPack(t.Context(), root, "my-pack", false)
	if err == nil {
		t.Fatal("a fresh scaffold passed")
	}
	for _, want := range []string{
		"scenario.yaml: replace the parts marked TODO",
		"README.md: replace the parts marked TODO",
		"eval/my-pack-act: case.yaml: truth.root_causes needs a class",
		"eval/my-pack-look-alike: case.yaml: replace the parts marked TODO",
		"eval: no look-alike",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error doesn't say %q:\n%v", want, err)
		}
	}
	for _, name := range []string{"my-pack", "My_Pack"} {
		if err := eval.Scaffold(root, name); err == nil {
			t.Errorf("Scaffold(%q) made a pack", name)
		}
	}
}

// A public corpus goes through the same redaction as production evidence, and
// never holds what a template wouldn't return.
func TestBuild_RedactsAndLeaksNothing(t *testing.T) {
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := eval.Load(oom, rootCauses(t))
	if err != nil {
		t.Fatal(err)
	}
	rollout, _ := cat.Profile("rollout-regression")
	b, err := eval.Build(t.Context(), oom, s, os.DirFS(packs), rollout)
	if err != nil {
		t.Fatal(err)
	}
	data, err := eval.Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fixture-s3cr3t-do-not-leak") {
		t.Error("the bundle holds a secret planted in the fixture")
	}
	logs, ok := b.Requestable[evidence.ContainerLogsTail]
	if !ok || logs.Redactions == 0 {
		t.Errorf("logs = %+v, want the planted password redacted", logs)
	}
	if logs.Classification != string(evidence.ClassificationSensitive) {
		t.Errorf("logs are classified %s", logs.Classification)
	}
}

// Initial evidence the core can't read is a gap in the bundle, as it is in
// production, not a failed build.
func TestBuild_RecordsInitialGaps(t *testing.T) {
	dir := t.TempDir()
	cluster := `apiVersion: apps/v1
kind: Deployment
metadata: {name: checkout}
spec:
  selector: {matchLabels: {app: checkout}}
  template:
    metadata: {labels: {app: checkout}}
    spec: {containers: [{name: app, image: "checkout:2"}]}
`
	if err := os.WriteFile(filepath.Join(dir, "cluster.yaml"), []byte(cluster), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	rollout, _ := cat.Profile("rollout-regression")
	read := []string{evidence.PodStates, evidence.ContainerLogsTail}
	s := eval.Case{
		Now:      time.Date(2026, 10, 27, 9, 20, 0, 0, time.UTC),
		Target:   eval.Target{Kind: "Deployment", Namespace: "shop", Name: "checkout"},
		Evidence: eval.Evidence{Initial: read, Allowed: read},
	}
	b, err := eval.Build(t.Context(), dir, s, os.DirFS(packs), rollout)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Initial) != 1 || b.Initial[0].ID != "ev-"+evidence.PodStates {
		t.Errorf("initial = %+v, want pod_states alone", b.Initial)
	}
	if len(b.Gaps) != 1 || b.Gaps[0].Template != evidence.ContainerLogsTail || b.Gaps[0].Cause != string(evidence.GapAbsent) {
		t.Errorf("gaps = %+v, want container_logs_tail absent: there are no pods", b.Gaps)
	}
	// With no earlier revision, no rollback reaches anyone.
	if b.Policy.Outcome != "deny" {
		t.Errorf("policy = %+v, want deny: there is nothing to roll back to", b.Policy)
	}
}

// rootCauses are the rollout-regression pack's.
func rootCauses(t *testing.T) []string {
	t.Helper()
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cat.RootCauses.For("rollout-regression") {
		names = append(names, c.Name)
	}
	return names
}

func TestCase_Validate(t *testing.T) {
	causes := rootCauses(t)
	good := func() eval.Case {
		s, err := eval.Load(oom, causes)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tests := []struct {
		name string
		edit func(*eval.Case)
		want string
	}{
		{"no version", func(s *eval.Case) { s.Version = 0 }, "version"},
		{"unknown difficulty", func(s *eval.Case) { s.Difficulty = "brutal" }, "difficulty"},
		{"unknown decision", func(s *eval.Case) { s.Truth.Decision = "guess" }, "truth.decision"},
		{"no right cause", func(s *eval.Case) { s.Truth.RootCauses = nil }, "root_causes"},
		{
			"cause the pack doesn't declare", func(s *eval.Case) { s.Truth.RootCauses = []string{"cosmic_rays"} },
			"truth.root_causes: cosmic_rays isn't one of the root causes in the pack's scenario.yaml",
		},
		{"propose with no revision", func(s *eval.Case) { s.Truth.AcceptableRevisions = nil }, "acceptable_revisions"},
		{"right and harmful", func(s *eval.Case) { s.Truth.RollbackIsHarmful = true }, "right and harmful"},
		{"initial not allowed", func(s *eval.Case) { s.Evidence.Initial = []string{"nope"} }, "evidence.initial"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := good()
			tt.edit(&s)
			if err := s.Validate(causes); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
