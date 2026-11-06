// Package modeltest tests the model's connection for the Setup page. Only the
// investigator may reach the model, so a test waits for the investigator to
// take it, make one small request with the settings under test, and report
// what happened. Tests are kept in memory: the core runs as one replica.
package modeltest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hzerrad/themis/internal/config/settings"
)

const (
	// timeout is how long the model has to answer.
	timeout = 20 * time.Second
	// pickup is how long a test waits for an investigator to take it. An
	// investigator that is connected is waiting for one, and takes it at once.
	// With the report's wait, a test ends within 27 s, inside the API's 30 s
	// to answer.
	pickup = 2 * time.Second
	// hold is how long an investigator's request waits for a test.
	hold = 10 * time.Second
)

// reportWithin is how long the investigator has to report a test whose model
// has timeout to answer: a quarter more, 25 s by default.
func reportWithin(timeout time.Duration) time.Duration { return timeout + timeout/4 }

// ErrBusy means another test is running.
var ErrBusy = errors.New("a test of the model's connection is already running")

// ErrUnknown means a report came for a test no one waits for any more.
var ErrUnknown = errors.New("no test is waiting with this id")

// Test is a test as the investigator takes it.
type Test struct {
	ID string
	// Settings are the settings under test, as setup would hand them, the key
	// in the clear. The investigator's own environment wins where it sets
	// them, as in an investigation.
	Settings settings.Model
	// Timeout is how long the model has to answer.
	Timeout time.Duration
}

// Tests hands tests to investigators, one at a time.
type Tests struct {
	// Timeout, Pickup and Hold replace the defaults above, for tests.
	Timeout, Pickup, Hold time.Duration

	offers    chan *test
	closed    chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	current *test
}

type test struct {
	Test
	done chan Result
}

// New returns Tests with no test running.
func New() *Tests {
	return &Tests{offers: make(chan *test), closed: make(chan struct{})}
}

// Run hands a test of m to an investigator and waits for its result. ErrBusy
// means another test is running. Without an investigator to take it, the
// result says so.
func (t *Tests) Run(ctx context.Context, m settings.Model) (Result, error) {
	run := &test{Test: Test{ID: uuid.NewString(), Settings: m, Timeout: or(t.Timeout, timeout)}, done: make(chan Result, 1)}
	t.mu.Lock()
	if t.current != nil {
		t.mu.Unlock()
		return Result{}, ErrBusy
	}
	t.current = run
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.current = nil
		t.mu.Unlock()
	}()

	taken := time.NewTimer(or(t.Pickup, pickup))
	defer taken.Stop()
	select {
	case t.offers <- run:
	case <-taken.C:
		return Result{Outcome: NoInvestigator}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	wait := reportWithin(run.Timeout)
	reported := time.NewTimer(wait)
	defer reported.Stop()
	select {
	case r := <-run.done:
		return r, nil
	case <-reported.C:
		return Result{Outcome: NoReport, Latency: wait}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Take waits for a test until one comes, ctx ends, the hold passes or the
// core stops. False means none came.
func (t *Tests) Take(ctx context.Context) (Test, bool) {
	held := time.NewTimer(or(t.Hold, hold))
	defer held.Stop()
	select {
	case run := <-t.offers:
		return run.Test, true
	case <-held.C:
	case <-ctx.Done():
	case <-t.closed:
	}
	return Test{}, false
}

// Report gives the running test with id its result. A second report is
// ignored.
func (t *Tests) Report(id string, r Result) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil || t.current.ID != id {
		return ErrUnknown
	}
	select {
	case t.current.done <- r:
	default:
	}
	return nil
}

// Close ends every Take waiting, so the core stops without waiting for them.
func (t *Tests) Close() {
	t.closeOnce.Do(func() { close(t.closed) })
}

func or(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}
