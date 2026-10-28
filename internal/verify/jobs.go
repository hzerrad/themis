// Package verify measures whether an executed action fixed its incident.
package verify

import "time"

// VerifyArgs verify an executed action over a window starting at
// WindowStart, which is persisted so a restart does not restart the window.
type VerifyArgs struct {
	ActionID    string    `json:"actionID"`
	WindowStart time.Time `json:"windowStart"`
}

// Kind is persisted with each job and must never change.
func (VerifyArgs) Kind() string { return "verify" }
