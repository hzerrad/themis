// Package catalog is everything this install of Themis can read, check and
// change: its evidence templates, checks and fixes, and the scenario packs
// that bring checks, profiles and root causes. It is built once, and each
// part goes to the code that uses it.
package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/probes"
	"github.com/hzerrad/themis/internal/catalog/capability/resources"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/scenarios"
)

// Catalog is what an install can read, check and change.
type Catalog struct {
	Fixes    capability.Registry
	Evidence evidence.Registry
	Checks   check.List

	// Profiles are the scenario packs' profiles, in the packs' name order.
	// A profile of the install's own with the same name replaces one.
	Profiles []config.Profile
	// RootCauses are the root causes each pack can diagnose.
	RootCauses RootCauses
}

// Profile is the profile of the scenario pack named.
func (c Catalog) Profile(name string) (config.Profile, bool) {
	i := slices.IndexFunc(c.Profiles, func(p config.Profile) bool { return p.Name == name })
	if i < 0 {
		return config.Profile{}, false
	}
	return c.Profiles[i], true
}

// RootCauses are each scenario pack's root causes, by the pack's name, which
// is its profile's.
type RootCauses map[string][]RootCause

// For is the root causes an investigation under the profiles named may name,
// each once, in the order named: each profile's pack's, or, for a profile of
// the install's own with a name no pack has, every pack's, in the packs' name
// order. A name two lists share, such as other, keeps its first place.
func (r RootCauses) For(profiles ...string) []RootCause {
	var out []RootCause
	add := func(causes []RootCause) {
		for _, c := range causes {
			if !slices.ContainsFunc(out, func(o RootCause) bool { return o.Name == c.Name }) {
				out = append(out, c)
			}
		}
	}
	for _, profile := range profiles {
		if causes, ok := r[profile]; ok {
			add(causes)
			continue
		}
		for _, pack := range slices.Sorted(maps.Keys(r)) {
			add(r[pack])
		}
	}
	return out
}

// Themis is what Themis ships with, every scenario pack on, and no metrics
// source. logs reads containers' logs for the container-logs template.
func Themis(logs evidence.LogReader) (Catalog, error) {
	return Build(logs, nil, scenarios.FS, nil)
}

// Build is Themis's evidence templates and fixes, with the scenario packs in
// packs, one folder each, except those disabled names. logs reads
// containers' logs, and usage what they use, for the templates that need
// them.
//
// Every pack's checks are compiled and its profile checked here, so a broken
// pack stops Themis from starting, with a message naming it. So does a
// disabled name no pack has.
func Build(logs evidence.LogReader, usage metrics.Source, packs fs.FS, disabled []string) (Catalog, error) {
	templates := evidence.NewRegistry(
		evidence.NewRolloutHistory(),
		evidence.NewPodStates(),
		evidence.NewTargetEvents(),
		evidence.NewReferencedConfig(),
		evidence.NewContainerLogsTail(logs),
		evidence.NewContainerMemory(usage),
		evidence.NewContainerCPU(usage),
	)
	found, err := readPacks(packs)
	if err != nil {
		return Catalog{}, err
	}
	enabled, err := enable(found, disabled)
	if err != nil {
		return Catalog{}, err
	}
	c := Catalog{Fixes: Fixes(), Evidence: templates, RootCauses: RootCauses{}}
	if err := c.add(enabled); err != nil {
		return Catalog{}, err
	}
	return c, nil
}

// add compiles each pack's checks into c, then checks its profile against
// everything c has, and adds it.
func (c *Catalog) add(packs []pack) error {
	var errs []error
	names := slices.Sorted(maps.Keys(c.Evidence))
	definedBy := map[string]string{}
	for _, p := range packs {
		list, err := check.Compile(p.checks, names)
		if err != nil {
			errs = append(errs, fmt.Errorf("scenario pack %s: %w", p.name, err))
			continue
		}
		for _, ch := range list {
			if other, ok := definedBy[ch.Name]; ok {
				errs = append(errs, fmt.Errorf("scenario pack %s: check %s: already defined by the pack %s", p.name, ch.Name, other))
				continue
			}
			definedBy[ch.Name] = p.name
			c.Checks = append(c.Checks, ch)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	priorityOf := map[int]string{}
	for _, p := range packs {
		if err := c.CheckProfile(p.profile); err != nil {
			errs = append(errs, fmt.Errorf("scenario pack %s: profile: %w", p.name, err))
			continue
		}
		if other, ok := priorityOf[p.profile.Priority]; ok {
			errs = append(errs, fmt.Errorf("scenario pack %s: profile: priority %d is already used by the pack %s", p.name, p.profile.Priority, other))
			continue
		}
		priorityOf[p.profile.Priority] = p.name
		c.Profiles = append(c.Profiles, p.profile)
		c.RootCauses[p.name] = p.rootCauses
	}
	return errors.Join(errs...)
}

// Fixes is the fixes Themis ships with, for the executor, which needs no
// more of the catalog.
func Fixes() capability.Registry {
	return capability.NewRegistry(rollback.Capability{}, probes.Capability{}, resources.Capability{})
}

// CheckProfile says what in p this catalog can't do, one error per problem:
// evidence templates, checks and fixes it doesn't have, and a policy that
// doesn't compile.
func (c Catalog) CheckProfile(p config.Profile) error {
	var errs []error
	var named []string
	for _, name := range slices.Concat(p.Evidence.Initial, p.Evidence.Allowed) {
		if _, ok := c.Evidence[name]; !ok && !slices.Contains(named, name) {
			errs = append(errs, fmt.Errorf("unknown evidence template %q", name))
		}
		named = append(named, name)
	}
	for _, name := range p.Checks {
		if _, ok := c.Checks.Get(name); !ok {
			errs = append(errs, fmt.Errorf("unknown check %q", name))
		}
	}
	for _, name := range p.Capabilities {
		if _, ok := c.Fixes[name]; !ok {
			errs = append(errs, fmt.Errorf("unknown capability %q", name))
		}
	}
	compiled, err := policy.Compile(p.EffectivePolicy())
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	// A rule reading a check that doesn't run can't be evaluated, and a rule
	// that can't be evaluated denies: every fix would be refused.
	for _, r := range compiled.Reads() {
		if _, ok := c.Checks.Get(r.Check); !ok {
			errs = append(errs, fmt.Errorf("rule %s reads the check %q, which doesn't exist", r.Rule, r.Check))
		} else if len(p.Checks) > 0 && !slices.Contains(p.Checks, r.Check) {
			errs = append(errs, fmt.Errorf("rule %s reads the check %q, which the profile doesn't run", r.Rule, r.Check))
		}
	}
	return errors.Join(errs...)
}

// CheckSources says which fixes outside agents are allowed that this catalog
// doesn't have.
func (c Catalog) CheckSources(e config.ExternalProposals) error {
	var errs []error
	for i, s := range e.Sources {
		for j, name := range s.Capabilities {
			if _, ok := c.Fixes[name]; !ok {
				errs = append(errs, fmt.Errorf("sources[%d].capabilities[%d]: unknown capability %q", i, j, name))
			}
		}
	}
	return errors.Join(errs...)
}

// CheckConfig checks every profile and outside agent in cfg, naming each
// profile with a problem.
func (c Catalog) CheckConfig(cfg config.Config) error {
	var errs []error
	for _, p := range cfg.Profiles {
		if err := c.CheckProfile(p); err != nil {
			errs = append(errs, fmt.Errorf("profile %s: %w", p.Name, err))
		}
	}
	if err := c.CheckSources(cfg.ExternalProposals); err != nil {
		errs = append(errs, fmt.Errorf("externalProposals: %w", err))
	}
	return errors.Join(errs...)
}
