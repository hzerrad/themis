// Package verify measures whether an executed action fixed its incident.
package verify

import (
	"time"

	"github.com/riverqueue/river"
)

// VerifyArgs verify an executed action over a window starting at
// WindowStart, which is persisted so a restart does not restart the window.
type VerifyArgs struct {
	ActionID    string    `json:"actionID"`
	WindowStart time.Time `json:"windowStart"`
}

// Kind is persisted with each job and must never change.
func (VerifyArgs) Kind() string { return "verify" }

// InsertOpts give up observing after about 4 hours of failures; snoozes do
// not count.
func (VerifyArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 10}
}
