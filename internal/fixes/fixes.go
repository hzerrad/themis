// Package fixes registers the fixes Themis ships with.
package fixes

import (
	"github.com/hzerrad/themis/internal/fix"
	"github.com/hzerrad/themis/internal/rollback"
)

// Default is every fix Themis ships with.
func Default() fix.Registry {
	return fix.NewRegistry(rollback.Fix{})
}
