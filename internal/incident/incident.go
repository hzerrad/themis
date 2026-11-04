package incident

import (
	"regexp"
	"strings"
)

type TargetRef struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	UID       string
}

// KindGroup is the target's kind and API group, as Kind.group, or Kind alone
// for the core group.
func (t TargetRef) KindGroup() string {
	if t.Group == "" {
		return t.Kind
	}
	return t.Kind + "." + t.Group
}

// KindGroupPattern matches Kind.group, with no group for the core group:
// Deployment.apps, Rollout.argoproj.io, PersistentVolumeClaim.
const KindGroupPattern = `^[A-Z][A-Za-z0-9]*(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)?$`

var kindGroupRE = regexp.MustCompile(KindGroupPattern)

// IsKindGroup reports whether s is a well-formed Kind.group.
func IsKindGroup(s string) bool {
	return kindGroupRE.MatchString(s)
}

// Fingerprint identifies an incident as profile/Kind.group/namespace/name/uid.
// It is stored, so the format must not change.
func Fingerprint(profile string, t TargetRef) string {
	return strings.Join([]string{profile, t.KindGroup(), t.Namespace, t.Name, t.UID}, "/")
}
