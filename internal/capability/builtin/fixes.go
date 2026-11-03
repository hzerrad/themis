// Package builtin registers the fixes Themis ships with.
package builtin

import (
	"github.com/hzerrad/themis/internal/capability"
	"github.com/hzerrad/themis/internal/capability/rollback"
)

// Default is every fix Themis ships with.
func Default() capability.Registry {
	return capability.NewRegistry(rollback.Capability{})
}
