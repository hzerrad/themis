package intake

import (
	"strings"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
)

// Fingerprint identifies an incident as profile/Kind.group/namespace/name/uid.
// It is stored, so the format must not change.
func Fingerprint(profile string, t incident.TargetRef) string {
	kindGroup := config.TargetKind(t)
	return strings.Join([]string{profile, kindGroup, t.Namespace, t.Name, t.UID}, "/")
}
