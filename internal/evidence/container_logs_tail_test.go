package evidence_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
)

// fakeLogs serves logs by "pod/container", and "pod/container/previous" for
// the previous run.
type fakeLogs map[string]string

func (f fakeLogs) Logs(_ context.Context, req evidence.LogRequest) (io.ReadCloser, error) {
	key := req.Pod + "/" + req.Container
	if req.Previous {
		key += "/previous"
	}
	logs, ok := f[key]
	if !ok {
		return nil, errors.New("no logs")
	}
	return io.NopCloser(strings.NewReader(logs)), nil
}

func collectLogs(t *testing.T, logs fakeLogs, target incident.TargetRef, params map[string]string) evidence.Item {
	t.Helper()
	item, err := evidence.NewContainerLogsTail(logs).Collect(t.Context(), k8s, target, params)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !json.Valid(item.Data) {
		t.Fatalf("Data is not valid JSON: %s", item.Data)
	}
	return item
}

// Without parameters, the logs are those of the pod needing attention, from
// the run that crashed.
func TestLogsTail_DefaultsToTheCrashedRunOfTheWorstPod(t *testing.T) {
	logs := fakeLogs{
		"checkout-7d9f8b6c4-x2kqp/app/previous": "loading catalog into memory\n",
		"checkout-5c6a1e2f3-a1b2c/app":          "serving\n",
	}
	data := string(collectLogs(t, logs, loadScenario1(t), nil).Data)
	if !strings.Contains(data, "loading catalog into memory") {
		t.Errorf("Data lacks the crashed run's logs: %s", data)
	}
	if !strings.Contains(data, "checkout-7d9f8b6c4-x2kqp") {
		t.Errorf("Data does not name the pod: %s", data)
	}
}

func TestLogsTail_NamedPod(t *testing.T) {
	logs := fakeLogs{"checkout-5c6a1e2f3-a1b2c/app": "serving\n"}
	data := string(collectLogs(t, logs, loadScenario1(t), map[string]string{"pod": "checkout-5c6a1e2f3-a1b2c"}).Data)
	if !strings.Contains(data, "serving") {
		t.Errorf("Data lacks the named pod's logs: %s", data)
	}
}

// The pod parameter comes from the model: it cannot reach another workload.
func TestLogsTail_PodOfAnotherWorkloadRefused(t *testing.T) {
	logs := fakeLogs{"cart-6b7c8d9e0-z9y8x/app": "cart secrets\n"}
	_, err := evidence.NewContainerLogsTail(logs).Collect(t.Context(), k8s, loadScenario1(t), map[string]string{"pod": "cart-6b7c8d9e0-z9y8x"})
	if !errors.Is(err, evidence.ErrInvalidParams) {
		t.Errorf("err = %v, want ErrInvalidParams", err)
	}
}

func TestLogsTail_TruncatesAndKeepsUTF8(t *testing.T) {
	var big bytes.Buffer
	for big.Len() < 5<<20 {
		big.WriteString("line of log output \xff\xfe é\n")
	}
	big.WriteString("the last line")
	logs := fakeLogs{"checkout-7d9f8b6c4-x2kqp/app/previous": big.String()}

	item := collectLogs(t, logs, loadScenario1(t), nil)
	if len(item.Data) > evidence.MaxLogBytes+1024 {
		t.Errorf("len(Data) = %d, want about %d", len(item.Data), evidence.MaxLogBytes)
	}
	if !item.Truncated {
		t.Error("Truncated = false")
	}
	if item.OriginalBytes <= evidence.MaxLogBytes {
		t.Errorf("OriginalBytes = %d", item.OriginalBytes)
	}
	var view map[string]any
	if err := json.Unmarshal(item.Data, &view); err != nil {
		t.Fatal(err)
	}
	for _, v := range view {
		if s, ok := v.(string); ok && !utf8.ValidString(s) {
			t.Error("Data holds invalid UTF-8")
		}
	}
	if !strings.Contains(string(item.Data), "the last line") {
		t.Error("the tail was not kept")
	}
}

func TestLogsTail_IsSensitive(t *testing.T) {
	logs := fakeLogs{"checkout-7d9f8b6c4-x2kqp/app/previous": "x\n"}
	if c := collectLogs(t, logs, loadScenario1(t), nil).Classification; c != evidence.ClassificationSensitive {
		t.Errorf("Classification = %q, want sensitive", c)
	}
}
