package intake

import "testing"

// The kind is stored with every queued job; renaming it strands them.
func TestProcessAlertArgs_KindIsPinned(t *testing.T) {
	if got := (ProcessAlertArgs{}).Kind(); got != "process_alert" {
		t.Errorf("Kind = %q, want process_alert", got)
	}
}

// River's default of 25 attempts would retry a stale alert for about 18 days.
func TestProcessAlertArgs_BoundedAttempts(t *testing.T) {
	if got := (ProcessAlertArgs{}).InsertOpts().MaxAttempts; got != 10 {
		t.Errorf("MaxAttempts = %d, want 10 (about 4 hours of retries)", got)
	}
}
