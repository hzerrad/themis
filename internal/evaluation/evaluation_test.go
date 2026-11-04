package evaluation_test

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/evaluation"
	"github.com/hzerrad/themis/internal/evidence"
)

var update = flag.Bool("update", false, "rewrite each case's bundle.json")

const (
	packs = "../../scenarios"
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
			r, err := evaluation.CheckPack(t.Context(), packs, e.Name(), *update)
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
	if err := evaluation.Scaffold(root, "my-pack"); err != nil {
		t.Fatal(err)
	}
	_, err := evaluation.CheckPack(t.Context(), root, "my-pack", false)
	if err == nil {
		t.Fatal("a fresh scaffold passed")
	}
	for _, want := range []string{
		"scenario.yaml: replace the parts marked TODO",
		"README.md: replace the parts marked TODO",
		"eval/my-pack-act: case.yaml: truth.root_causes needs a class",
		"eval/my-pack-look-alike: case.yaml: replace the parts marked TODO",
		"eval: no case where acting is right",
		"eval: no look-alike",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error doesn't say %q:\n%v", want, err)
		}
	}
	for _, name := range []string{"my-pack", "My_Pack"} {
		if err := evaluation.Scaffold(root, name); err == nil {
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
	s, err := evaluation.Load(oom, rootCauses(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := evaluation.Build(t.Context(), oom, s, os.DirFS(packs), cat.Profiles[0])
	if err != nil {
		t.Fatal(err)
	}
	data, err := evaluation.Encode(b)
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
	good := func() evaluation.Case {
		s, err := evaluation.Load(oom, causes)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tests := []struct {
		name string
		edit func(*evaluation.Case)
		want string
	}{
		{"no version", func(s *evaluation.Case) { s.Version = 0 }, "version"},
		{"unknown difficulty", func(s *evaluation.Case) { s.Difficulty = "brutal" }, "difficulty"},
		{"unknown decision", func(s *evaluation.Case) { s.Truth.Decision = "guess" }, "truth.decision"},
		{"no right cause", func(s *evaluation.Case) { s.Truth.RootCauses = nil }, "root_causes"},
		{
			"cause the pack doesn't declare", func(s *evaluation.Case) { s.Truth.RootCauses = []string{"cosmic_rays"} },
			"truth.root_causes: cosmic_rays isn't one of the root causes in the pack's scenario.yaml",
		},
		{"propose with no revision", func(s *evaluation.Case) { s.Truth.AcceptableRevisions = nil }, "acceptable_revisions"},
		{"right and harmful", func(s *evaluation.Case) { s.Truth.RollbackIsHarmful = true }, "right and harmful"},
		{"initial not allowed", func(s *evaluation.Case) { s.Evidence.Initial = []string{"nope"} }, "evidence.initial"},
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
