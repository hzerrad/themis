package intake_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/intake"
)

var checkout = incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "5f1c2a7e"}

// The format is pinned: stored fingerprints must keep matching new alerts.
func TestFingerprint_Format(t *testing.T) {
	tests := []struct {
		name   string
		target incident.TargetRef
		want   string
	}{
		{"named group", checkout, "rollout-regression/Deployment.apps/shop/checkout/5f1c2a7e"},
		{"core group has no suffix", incident.TargetRef{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data", UID: "u2"}, "rollout-regression/PersistentVolumeClaim/shop/data/u2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := intake.Fingerprint("rollout-regression", tt.target); got != tt.want {
				t.Errorf("Fingerprint = %q, want %q", got, tt.want)
			}
		})
	}
}

// Changing any part of the incident's identity gives a different fingerprint.
func TestFingerprint_EveryPartCounts(t *testing.T) {
	base := intake.Fingerprint("rollout-regression", checkout)

	tests := []struct {
		name    string
		profile string
		mutate  func(t *incident.TargetRef)
	}{
		{"profile", "dependency-failure", func(*incident.TargetRef) {}},
		{"group", "rollout-regression", func(t *incident.TargetRef) { t.Group = "example.com" }},
		{"kind", "rollout-regression", func(t *incident.TargetRef) { t.Kind = "StatefulSet" }},
		{"namespace", "rollout-regression", func(t *incident.TargetRef) { t.Namespace = "payments" }},
		{"name", "rollout-regression", func(t *incident.TargetRef) { t.Name = "cart" }},
		{"recreated object", "rollout-regression", func(t *incident.TargetRef) { t.UID = "9b0d4e11" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := checkout
			tt.mutate(&target)

			if got := intake.Fingerprint(tt.profile, target); got == base {
				t.Errorf("Fingerprint unchanged (%q) after changing the %s", got, tt.name)
			}
		})
	}
}
