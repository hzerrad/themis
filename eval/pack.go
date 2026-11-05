package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/config"
)

// PackReport is what CheckPack found in a scenario pack.
type PackReport struct {
	// Cases are the pack's evaluation cases.
	Cases int
	// Act are the cases where acting is right.
	Act int
	// LookAlikes are the cases where acting is wrong.
	LookAlikes int
}

// CheckPack checks the scenario pack root/name as `make scenario-check` does,
// reporting every problem, each in words a contributor can act on: its files
// are there, its profile and checks load with every other pack, it has a case
// where acting is right and a look-alike where it is wrong, and each case's
// checks give its right answers and its bundle is up to date. With update, it
// rewrites each bundle instead of comparing it.
//
// Replaying the cases through the model's saved replies is the evaluation's,
// in Python.
func CheckPack(ctx context.Context, root, name string, update bool) (PackReport, error) {
	dir := filepath.Join(root, name)
	var r PackReport
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	for _, f := range []struct{ name, holds string }{
		{"scenario.yaml", "the pack's profile"},
		{"README.md", "what the scenario covers, its evidence, its fix and its look-alikes"},
	} {
		if err := filled(filepath.Join(dir, f.name)); errors.Is(err, fs.ErrNotExist) {
			add("%s is missing: it says %s", f.name, f.holds)
		} else if err != nil {
			add("%s: %w", f.name, err)
		}
	}
	cases, err := os.ReadDir(filepath.Join(dir, "eval"))
	if err != nil {
		add("eval/ is missing: add the cases that prove the pack")
	}

	packs := os.DirFS(root)
	cat, err := catalog.Build(nil, nil, packs, nil)
	if err != nil {
		return r, errors.Join(append(errs, err)...)
	}
	profile, ok := cat.Profile(name)
	if !ok {
		return r, errors.Join(append(errs, fmt.Errorf("no scenario pack is named %s", name))...)
	}
	var causes []string
	for _, c := range cat.RootCauses.For(name) {
		causes = append(causes, c.Name)
	}

	for _, e := range cases {
		if !e.IsDir() {
			continue
		}
		s, err := checkCase(ctx, filepath.Join(dir, "eval", e.Name()), packs, profile, causes, update)
		if err != nil {
			errs = append(errs, prefixed("eval/"+e.Name(), err))
			continue
		}
		r.Cases++
		if s.Truth.Decision == DecisionPropose {
			r.Act++
		}
		if s.Truth.Decision == DecisionAbstain || s.Truth.RollbackIsHarmful {
			r.LookAlikes++
		}
	}
	if r.Act == 0 {
		add("eval: no case where acting is right (truth.decision: propose)")
	}
	if r.LookAlikes == 0 {
		add("eval: no look-alike, a case that looks like this scenario but where acting is wrong (truth.decision: abstain)")
	}
	return r, errors.Join(errs...)
}

// checkCase builds the case in dir, of a pack declaring causes, and compares
// it with its answers and its committed bundle, or rewrites the bundle.
func checkCase(ctx context.Context, dir string, packs fs.FS, profile config.Profile, causes []string, update bool) (Case, error) {
	var errs []error
	if err := filled(filepath.Join(dir, "case.yaml")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("case.yaml: %w", err))
	}
	s, err := Load(dir, causes)
	if err != nil {
		return s, errors.Join(append(errs, err)...)
	}
	b, err := Build(ctx, dir, s, packs, profile)
	if err != nil {
		return s, errors.Join(append(errs, err)...)
	}
	got := map[string]Check{}
	for _, c := range b.Predicates {
		got[c.Name] = c
	}
	for _, name := range slices.Sorted(maps.Keys(s.Truth.Predicates)) {
		want := s.Truth.Predicates[name]
		if c, ok := got[name]; !ok {
			errs = append(errs, fmt.Errorf("truth.predicates: the pack's profile runs no check %s", name))
		} else if c.Value != want {
			errs = append(errs, fmt.Errorf("check %s = %s (%s), want %s", name, c.Value, c.Reason, want))
		}
	}
	data, err := Encode(b)
	if err != nil {
		return s, err
	}
	path := filepath.Join(dir, "bundle.json")
	if update {
		if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // a public fixture
			return s, err
		}
		return s, errors.Join(errs...)
	}
	committed, err := os.ReadFile(path) //nolint:gosec // a path in the repository
	switch {
	case err != nil:
		errs = append(errs, errors.New("bundle.json is missing: run `make eval-bundles`"))
	case !bytes.Equal(committed, data):
		errs = append(errs, errors.New("bundle.json is stale: run `make eval-bundles` and review the change"))
	}
	return s, errors.Join(errs...)
}

// prefixed puts p in front of each of err's lines, so every problem in a
// joined error says where it is.
func prefixed(p string, err error) error {
	var out []error
	for line := range strings.SplitSeq(err.Error(), "\n") {
		out = append(out, fmt.Errorf("%s: %s", p, line))
	}
	return errors.Join(out...)
}

// filled says whether the file at path is there and has nothing left of the
// scaffold to fill in.
func filled(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // a path in the repository
	if err != nil {
		return err
	}
	if bytes.Contains(data, []byte("TODO")) {
		return errors.New("replace the parts marked TODO")
	}
	return nil
}

// Encode writes b as bundle.json holds it.
func Encode(b Bundle) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
