package incident_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/incident"
)

var checkout = incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "5f1c2a7e"}

// The format is pinned, with every part of the identity in it: stored
// fingerprints must keep matching new alerts.
func TestFingerprint_Format(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		target  incident.TargetRef
		want    string
	}{
		{"named group", "rollout-regression", checkout, "rollout-regression/Deployment.apps/shop/checkout/5f1c2a7e"},
		{"core group has no suffix", "dependency-failure", incident.TargetRef{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data", UID: "u2"}, "dependency-failure/PersistentVolumeClaim/shop/data/u2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := incident.Fingerprint(tt.profile, tt.target); got != tt.want {
				t.Errorf("Fingerprint = %q, want %q", got, tt.want)
			}
		})
	}
}
