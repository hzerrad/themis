package evaluation_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/evaluation"
	"github.com/hzerrad/themis/internal/evidence"
)

var update = flag.Bool("update", false, "rewrite each scenario's bundle.json")

const scenarios = "../../eval/scenarios"

func dirs(t *testing.T) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(scenarios, "*"))
	if err != nil || len(found) == 0 {
		t.Fatalf("no scenarios in %s: %v", scenarios, err)
	}
	return found
}

func encode(t *testing.T, b evaluation.Bundle) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Each scenario's checks must give its right answers, with no model involved,
// and its committed bundle must be what the code produces today.
func TestScenarios(t *testing.T) {
	for _, dir := range dirs(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			s, err := evaluation.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			b, err := evaluation.Build(t.Context(), dir, s)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]evaluation.Check{}
			for _, c := range b.Predicates {
				got[c.Name] = c
			}
			for name, want := range s.Truth.Predicates {
				c, ok := got[name]
				if !ok {
					t.Errorf("%s: no such check", name)
				} else if c.Value != want {
					t.Errorf("%s = %s (%s), want %s", name, c.Value, c.Reason, want)
				}
			}

			path := filepath.Join(dir, "bundle.json")
			data := encode(t, b)
			if *update {
				if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // a public fixture
					t.Fatal(err)
				}
				return
			}
			committed, err := os.ReadFile(path) //nolint:gosec // a path in the repository
			if err != nil {
				t.Fatalf("%v: run `make eval-bundles`", err)
			}
			if !bytes.Equal(committed, data) {
				t.Errorf("%s is stale: run `make eval-bundles` and review the change", path)
			}
		})
	}
}

func TestBuild_IsTheSameEveryTime(t *testing.T) {
	for _, dir := range dirs(t) {
		s, err := evaluation.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		first, err := evaluation.Build(t.Context(), dir, s)
		if err != nil {
			t.Fatal(err)
		}
		second, err := evaluation.Build(t.Context(), dir, s)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encode(t, first), encode(t, second)) {
			t.Errorf("%s: two builds differ", s.Name)
		}
	}
}

// A public corpus goes through the same redaction as production evidence, and
// never holds what a template wouldn't return.
func TestBuild_RedactsAndLeaksNothing(t *testing.T) {
	const dir = scenarios + "/rollout-oom"
	s, err := evaluation.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := evaluation.Build(t.Context(), dir, s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encode(t, b)), "fixture-s3cr3t-do-not-leak") {
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

func TestScenario_Validate(t *testing.T) {
	good := func() evaluation.Scenario {
		s, err := evaluation.Load(scenarios + "/rollout-oom")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tests := []struct {
		name string
		edit func(*evaluation.Scenario)
		want string
	}{
		{"no version", func(s *evaluation.Scenario) { s.Version = 0 }, "version"},
		{"unknown difficulty", func(s *evaluation.Scenario) { s.Difficulty = "brutal" }, "difficulty"},
		{"unknown decision", func(s *evaluation.Scenario) { s.Truth.Decision = "guess" }, "truth.decision"},
		{"no right cause", func(s *evaluation.Scenario) { s.Truth.RootCauses = nil }, "root_causes"},
		{"propose with no revision", func(s *evaluation.Scenario) { s.Truth.AcceptableRevisions = nil }, "acceptable_revisions"},
		{"right and harmful", func(s *evaluation.Scenario) { s.Truth.RollbackIsHarmful = true }, "right and harmful"},
		{"initial not allowed", func(s *evaluation.Scenario) { s.Evidence.Initial = []string{"nope"} }, "evidence.initial"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := good()
			tt.edit(&s)
			if err := s.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
