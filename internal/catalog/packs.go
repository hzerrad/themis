package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"

	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
)

// pack is one scenario pack, as read from its folder.
type pack struct {
	name       string
	profile    config.Profile
	checks     []check.Definition
	rootCauses []RootCause
}

// packFile is a pack's scenario.yaml.
type packFile struct {
	// Name is the pack's, and its folder's.
	Name string `json:"name"`
	// Description says in plain words which incidents the pack handles.
	Description string `json:"description"`
	// RootCauses are the root causes the pack can diagnose, in the order an
	// investigator reads them.
	RootCauses []RootCause `json:"rootCauses"`
	// Profile is the pack's profile, named after the pack.
	Profile config.Profile `json:"profile"`
}

// RootCause is a kind of root cause a scenario pack can diagnose: what an
// investigator may name as the cause of an incident, and what the pack's
// cases name as the right one.
type RootCause struct {
	// Name is how a hypothesis names it: lowercase letters, digits and _.
	Name string `json:"name"`
	// Description says what it means, in plain words.
	Description string `json:"description"`
}

var rootCauseName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checkRootCauses reports every problem in a pack's root causes.
func checkRootCauses(causes []RootCause) error {
	if len(causes) == 0 {
		return errors.New("scenario.yaml: rootCauses needs a class")
	}
	var errs []error
	seen := map[string]bool{}
	for i, c := range causes {
		switch {
		case !rootCauseName.MatchString(c.Name):
			errs = append(errs, fmt.Errorf("scenario.yaml: rootCauses[%d]: name %q must be lowercase letters, digits and _", i, c.Name))
		case seen[c.Name]:
			errs = append(errs, fmt.Errorf("scenario.yaml: rootCauses[%d]: %s is listed twice", i, c.Name))
		case c.Description == "":
			errs = append(errs, fmt.Errorf("scenario.yaml: rootCauses[%d]: %s needs a description", i, c.Name))
		}
		seen[c.Name] = true
	}
	return errors.Join(errs...)
}

// readPacks reads every folder at the root of fsys as a pack, in name order,
// reporting every broken pack, not only the first.
func readPacks(fsys fs.FS) ([]pack, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read scenario packs: %w", err)
	}
	var out []pack
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := readPack(fsys, e.Name())
		if err != nil {
			errs = append(errs, fmt.Errorf("scenario pack %s: %w", e.Name(), err))
			continue
		}
		out = append(out, p)
	}
	return out, errors.Join(errs...)
}

func readPack(fsys fs.FS, dir string) (pack, error) {
	data, err := fs.ReadFile(fsys, path.Join(dir, "scenario.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return pack{}, errors.New("no scenario.yaml")
	}
	if err != nil {
		return pack{}, err
	}
	var f packFile
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return pack{}, fmt.Errorf("scenario.yaml: %w", err)
	}
	var errs []error
	if f.Name != dir {
		errs = append(errs, fmt.Errorf("scenario.yaml: name %q must be the folder's, %q", f.Name, dir))
	}
	if f.Description == "" {
		errs = append(errs, errors.New("scenario.yaml: description is required"))
	}
	if f.Profile.Name != "" && f.Profile.Name != dir {
		errs = append(errs, errors.New("scenario.yaml: profile.name must be the pack's name, or left out"))
	}
	if err := checkRootCauses(f.RootCauses); err != nil {
		errs = append(errs, err)
	}
	f.Profile.Name = dir
	if err := f.Profile.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("scenario.yaml: profile: %w", err))
	}
	p := pack{name: dir, profile: f.Profile, rootCauses: f.RootCauses}
	data, err = fs.ReadFile(fsys, path.Join(dir, "checks.yaml"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		errs = append(errs, err)
	default:
		if p.checks, err = check.Parse(data); err != nil {
			errs = append(errs, fmt.Errorf("checks.yaml: %w", err))
		}
	}
	return p, errors.Join(errs...)
}

// enable keeps the packs disabled doesn't name, and refuses a name no pack
// has.
func enable(packs []pack, disabled []string) ([]pack, error) {
	var errs []error
	for _, name := range disabled {
		if !slices.ContainsFunc(packs, func(p pack) bool { return p.name == name }) {
			errs = append(errs, fmt.Errorf("scenarios.disabled: no scenario pack is named %q", name))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return slices.DeleteFunc(slices.Clone(packs), func(p pack) bool { return slices.Contains(disabled, p.name) }), nil
}
