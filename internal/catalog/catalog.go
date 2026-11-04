// Package catalog is everything this install of Themis can read, check and
// change: its evidence templates, checks and fixes. It is built once, and each
// part goes to the code that uses it.
package catalog

import (
	"errors"
	"fmt"
	"slices"

	"github.com/hzerrad/themis/internal/capability"
	"github.com/hzerrad/themis/internal/capability/rollback"
	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/policy"
)

// Catalog is what an install can read, check and change.
type Catalog struct {
	Fixes    capability.Registry
	Evidence evidence.Registry
	Checks   check.List
}

// Themis is what Themis ships with. logs reads containers' logs for the
// container-logs template.
func Themis(logs evidence.LogReader) Catalog {
	return Catalog{
		Fixes: Fixes(),
		Evidence: evidence.NewRegistry(
			evidence.NewRolloutHistory(),
			evidence.NewPodStates(),
			evidence.NewTargetEvents(),
			evidence.NewReferencedConfig(),
			evidence.NewContainerLogsTail(logs),
		),
		Checks: check.Rollout(),
	}
}

// Fixes is the fixes Themis ships with, for the executor, which needs no
// more of the catalog.
func Fixes() capability.Registry {
	return capability.NewRegistry(rollback.Capability{})
}

// CheckProfile says what in p this catalog can't do, one error per problem:
// evidence templates and fixes it doesn't have, and a policy that doesn't
// compile.
func (c Catalog) CheckProfile(p config.Profile) error {
	var errs []error
	var named []string
	for _, name := range slices.Concat(p.Evidence.Initial, p.Evidence.Allowed) {
		if _, ok := c.Evidence[name]; !ok && !slices.Contains(named, name) {
			errs = append(errs, fmt.Errorf("unknown evidence template %q", name))
		}
		named = append(named, name)
	}
	for _, name := range p.Capabilities {
		if _, ok := c.Fixes[name]; !ok {
			errs = append(errs, fmt.Errorf("unknown capability %q", name))
		}
	}
	if _, err := policy.Compile(p.EffectivePolicy()); err != nil {
		errs = append(errs, err)
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
