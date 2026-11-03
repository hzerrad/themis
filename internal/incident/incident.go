package incident

import "strings"

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

// Fingerprint identifies an incident as profile/Kind.group/namespace/name/uid.
// It is stored, so the format must not change.
func Fingerprint(profile string, t TargetRef) string {
	return strings.Join([]string{profile, t.KindGroup(), t.Namespace, t.Name, t.UID}, "/")
}
