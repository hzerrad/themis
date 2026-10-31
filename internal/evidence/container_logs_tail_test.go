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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// Silence is not evidence: empty logs say so.
func TestLogsTail_EmptyLogsAreExplained(t *testing.T) {
	logs := fakeLogs{"checkout-7d9f8b6c4-x2kqp/app/previous": ""}
	data := string(collectLogs(t, logs, loadScenario1(t), nil).Data)
	if !strings.Contains(data, "neither confirms nor contradicts") {
		t.Errorf("empty logs carry no note: %s", data)
	}
}

// What the kubelet streams once the crashed pod's runs are gone.
const (
	previousGone = "unable to retrieve container logs for containerd://pppppppppppppppppppppppppppppppppppppppppppppppppppppppppppppppp"
	currentGone  = "unable to retrieve container logs for containerd://cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\n"
)

func logsView(t *testing.T, logs fakeLogs, params map[string]string) evidence.LogsView {
	t.Helper()
	var view evidence.LogsView
	if err := json.Unmarshal(collectLogs(t, logs, loadScenario1(t), params).Data, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// The kubelet's message is not the container's output: the current run is
// read instead.
func TestLogsTail_GonePreviousRunFallsBackToTheCurrentRun(t *testing.T) {
	view := logsView(t, fakeLogs{
		"checkout-7d9f8b6c4-x2kqp/app/previous": previousGone,
		"checkout-7d9f8b6c4-x2kqp/app":          "loading catalog into memory\n",
	}, nil)
	if view.Previous || view.Logs != "loading catalog into memory\n" {
		t.Errorf("got previous=%v logs=%q, want the current run's logs", view.Previous, view.Logs)
	}
	if !strings.Contains(view.Note, "these are the current run's") {
		t.Errorf("Note = %q, want it to say why the current run is shown", view.Note)
	}
}

func TestLogsTail_GoneLogsAreExplained(t *testing.T) {
	for name, c := range map[string]struct {
		logs   fakeLogs
		params map[string]string
		note   string
	}{
		"both runs gone": {
			fakeLogs{"checkout-7d9f8b6c4-x2kqp/app/previous": previousGone, "checkout-7d9f8b6c4-x2kqp/app": currentGone},
			nil,
			"nor of the current run",
		},
		"the asked run gone": {
			fakeLogs{"checkout-7d9f8b6c4-x2kqp/app/previous": previousGone},
			map[string]string{"previous": "true"},
			"the run that last ended",
		},
		"the current run gone": {
			fakeLogs{"checkout-7d9f8b6c4-x2kqp/app": currentGone},
			map[string]string{"previous": "false"},
			"the logs of the current run",
		},
	} {
		t.Run(name, func(t *testing.T) {
			view := logsView(t, c.logs, c.params)
			if view.Logs != "" || !strings.Contains(view.Note, c.note) {
				t.Errorf("got logs=%q note=%q, want no logs and a note saying %q", view.Logs, view.Note, c.note)
			}
			if strings.Contains(view.Note, "wrote no output") {
				t.Errorf("Note = %q: gone logs are not silence", view.Note)
			}
		})
	}
}

// Falling back to a current run that wrote nothing keeps both notes.
func TestLogsTail_EmptyCurrentRunAfterFallbackIsExplained(t *testing.T) {
	view := logsView(t, fakeLogs{
		"checkout-7d9f8b6c4-x2kqp/app/previous": previousGone,
		"checkout-7d9f8b6c4-x2kqp/app":          "",
	}, nil)
	if !strings.Contains(view.Note, "the run that last ended") || !strings.Contains(view.Note, "neither confirms nor contradicts") {
		t.Errorf("Note = %q, want the fallback and the empty-logs notes", view.Note)
	}
}

// A missing log file is reported with its path, which names the pod.
func TestLogsTail_GoneLogFileOfThisPodIsExplained(t *testing.T) {
	target := loadScenario1(t)
	var pod corev1.Pod
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: target.Namespace, Name: "checkout-7d9f8b6c4-x2kqp"}, &pod); err != nil {
		t.Fatal(err)
	}
	gone := `failed to open log file "/var/log/pods/` + target.Namespace + `_checkout-7d9f8b6c4-x2kqp_` + string(pod.UID) + `/app/7.log": open: no such file or directory`
	logs := fakeLogs{"checkout-7d9f8b6c4-x2kqp/app/previous": gone}
	item, err := evidence.NewContainerLogsTail(logs).Collect(t.Context(), k8s, target, map[string]string{"previous": "true"})
	if err != nil {
		t.Fatal(err)
	}
	var view evidence.LogsView
	if err := json.Unmarshal(item.Data, &view); err != nil {
		t.Fatal(err)
	}
	if view.Logs != "" || !strings.Contains(view.Note, "no longer has") {
		t.Errorf("got logs=%q note=%q, want no logs and a note", view.Logs, view.Note)
	}
}

// A container cannot hide its logs by printing the kubelet's words about
// another container.
func TestLogsTail_KubeletWordsAboutAnotherContainerAreLogs(t *testing.T) {
	for _, out := range []string{
		"unable to retrieve container logs for containerd://0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		`failed to open log file "/var/log/pods/shop_other_0000/app/0.log": open: no such file or directory`,
	} {
		view := logsView(t, fakeLogs{"checkout-7d9f8b6c4-x2kqp/app/previous": out}, nil)
		if !view.Previous || view.Logs != out {
			t.Errorf("got previous=%v logs=%q, want the output as logs", view.Previous, view.Logs)
		}
	}
}
