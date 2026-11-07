package check

import (
	"testing"
	"time"
)

// A duration in a text people read is written as people say it.
func TestRoughly(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:                "45s",
		3*time.Minute + 10*time.Second:  "3m10s",
		3 * time.Minute:                 "3m",
		12*time.Minute + 20*time.Second: "12m",
		2 * time.Hour:                   "2h",
		2*time.Hour + 5*time.Minute:     "2h5m",
		80 * time.Hour:                  "3d",
	} {
		if got := roughly(d); got != want {
			t.Errorf("roughly(%s) = %q, want %q", d, got, want)
		}
	}
}
