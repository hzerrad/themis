package modeltest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/server/modeltest"
)

// investigator takes one test and reports what report makes of it.
func investigator(t *testing.T, tests *modeltest.Tests, report func(modeltest.Test) modeltest.Result) <-chan modeltest.Test {
	t.Helper()
	took := make(chan modeltest.Test, 1)
	go func() {
		for {
			got, ok := tests.Take(context.Background())
			if !ok {
				continue
			}
			took <- got
			if report != nil {
				_ = tests.Report(got.ID, report(got))
			}
			return
		}
	}()
	return took
}

// A test goes to the investigator with the settings under test, key
// included, and its result comes back.
func TestRun_TheInvestigatorMakesTheTest(t *testing.T) {
	tests := modeltest.New()
	took := investigator(t, tests, func(modeltest.Test) modeltest.Result {
		return modeltest.Result{Outcome: modeltest.Connected, Model: "qwen3:8b", Latency: 1200 * time.Millisecond}
	})
	r, err := tests.Run(t.Context(), settings.Model{BaseURL: "http://ollama:11434/v1", Name: "qwen3:8b", APIKey: "sk-typed"})
	if err != nil || !r.OK() || r.Model != "qwen3:8b" {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	got := <-took
	if got.Settings.APIKey != "sk-typed" || got.Timeout != 20*time.Second || got.ID == "" {
		t.Errorf("the investigator took %+v", got)
	}
}

func TestRun_NoInvestigator(t *testing.T) {
	tests := modeltest.New()
	tests.Pickup = 20 * time.Millisecond
	r, err := tests.Run(t.Context(), settings.Model{})
	if err != nil || r.Outcome != modeltest.NoInvestigator {
		t.Errorf("Run = %+v, %v, want no investigator", r, err)
	}
}

// One test runs at a time; a report for a test no one waits for is refused.
func TestRun_OneAtATime(t *testing.T) {
	tests := modeltest.New()
	took := investigator(t, tests, nil)
	first := make(chan modeltest.Result, 1)
	go func() {
		r, _ := tests.Run(context.Background(), settings.Model{Name: "first"})
		first <- r
	}()
	got := <-took
	if _, err := tests.Run(t.Context(), settings.Model{Name: "second"}); !errors.Is(err, modeltest.ErrBusy) {
		t.Errorf("a second test: %v, want ErrBusy", err)
	}
	if err := tests.Report("another", modeltest.Result{}); !errors.Is(err, modeltest.ErrUnknown) {
		t.Errorf("a report for another test: %v, want ErrUnknown", err)
	}
	if err := tests.Report(got.ID, modeltest.Result{Outcome: modeltest.NoAnswer}); err != nil {
		t.Fatal(err)
	}
	if r := <-first; r.Outcome != modeltest.NoAnswer {
		t.Errorf("first = %+v", r)
	}
	if err := tests.Report(got.ID, modeltest.Result{}); !errors.Is(err, modeltest.ErrUnknown) {
		t.Errorf("a report after the test ended: %v, want ErrUnknown", err)
	}
}

// An investigator that takes a test and never reports doesn't hold the page
// past its timeout and a quarter.
func TestRun_NoReport(t *testing.T) {
	tests := modeltest.New()
	tests.Timeout = 40 * time.Millisecond
	investigator(t, tests, nil)
	r, err := tests.Run(t.Context(), settings.Model{})
	if err != nil || r.Outcome != modeltest.NoReport || r.Latency != 50*time.Millisecond {
		t.Errorf("Run = %+v, %v, want no report after 50ms", r, err)
	}
}

// Take waits for a test only so long, and not at all once the core stops.
func TestTake_EndsWithTheCore(t *testing.T) {
	tests := modeltest.New()
	tests.Hold = 20 * time.Millisecond
	if _, ok := tests.Take(t.Context()); ok {
		t.Fatal("took a test no one asked for")
	}
	tests.Hold = time.Hour
	tests.Close()
	done := make(chan bool)
	go func() { _, ok := tests.Take(context.Background()); done <- ok }()
	select {
	case ok := <-done:
		if ok {
			t.Error("took a test after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Take waited after Close")
	}
}
