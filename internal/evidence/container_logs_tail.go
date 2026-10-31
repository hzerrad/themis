package evidence

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/incident"
)

// ContainerLogsTail is the end of one container's logs, from a pod of the
// target.
const ContainerLogsTail = "container_logs_tail"

// MaxLogBytes bounds the logs of an item; the end is kept.
const MaxLogBytes = 16 << 10

// logTailLines are asked of the API server; at most maxLogReadBytes are read.
const (
	logTailLines    = 500
	maxLogReadBytes = 16 << 20
)

// ErrInvalidParams means a template's parameters are unknown or out of the
// target's scope.
var ErrInvalidParams = errors.New("invalid template parameters")

// LogRequest asks for the logs of one container.
type LogRequest struct {
	Namespace, Pod, Container string
	Previous                  bool
	TailLines                 int64
}

// LogReader reads container logs through the pods/log subresource.
type LogReader interface {
	Logs(ctx context.Context, req LogRequest) (io.ReadCloser, error)
}

// ClientsetLogs reads logs with a clientset.
type ClientsetLogs struct {
	Clientset kubernetes.Interface
}

func (c ClientsetLogs) Logs(ctx context.Context, req LogRequest) (io.ReadCloser, error) {
	return c.Clientset.CoreV1().Pods(req.Namespace).GetLogs(req.Pod, &corev1.PodLogOptions{
		Container: req.Container,
		Previous:  req.Previous,
		TailLines: ptr.To(req.TailLines),
	}).Stream(ctx)
}

type LogsView struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Previous  bool   `json:"previousRun"`
	Logs      string `json:"logs"`

	// Note explains empty logs, so silence is not read as evidence.
	Note string `json:"note,omitempty"`
}

// emptyLogsNote is the Note of a container that wrote nothing.
const emptyLogsNote = "The container wrote nothing. Processes killed for using too much memory often write nothing, so the silence proves nothing either way."

// The kubelet streams these, as if they were the logs, when it no longer has
// a run's logs: its container is gone, or its log file is.
var (
	goneContainer = regexp.MustCompile(`\Aunable to retrieve container logs for (\S+)\s*\z`)
	goneLogFile   = regexp.MustCompile(`\A(?:failed to resolve log path|failed to open log file|failed to try resolving symlinks in path) "([^"]+)"[^\n]*\s*\z`)
)

// logsGone reports whether raw is the kubelet saying it no longer has logs
// of this container. A container that prints the same words about another
// container is read as logs.
func logsGone(raw []byte, pod *corev1.Pod, status corev1.ContainerStatus) bool {
	if m := goneContainer.FindSubmatch(raw); m != nil {
		id := string(m[1])
		last := status.LastTerminationState.Terminated
		return id == status.ContainerID || (last != nil && id == last.ContainerID)
	}
	if m := goneLogFile.FindSubmatch(raw); m != nil {
		return strings.Contains(string(m[1]), "_"+string(pod.UID)+"/"+status.Name+"/")
	}
	return false
}

func goneNote(previous bool) string {
	if previous {
		return "the kubelet no longer has the logs of the run that last ended"
	}
	return "the kubelet no longer has the logs of the current run"
}

type containerLogsTail struct {
	logs LogReader
}

// NewContainerLogsTail returns the container_logs_tail template. Its optional
// parameters are pod, container and previous ("true" or "false"); by default
// it reads the pod needing attention most, from the run that last ended.
func NewContainerLogsTail(logs LogReader) Template { return containerLogsTail{logs: logs} }

func (containerLogsTail) Name() string { return ContainerLogsTail }

func (l containerLogsTail) Collect(ctx context.Context, c client.Reader, t incident.TargetRef, params map[string]string) (Item, error) {
	for k := range params {
		if k != "pod" && k != "container" && k != "previous" {
			return Item{}, fmt.Errorf("%w: unknown parameter %q", ErrInvalidParams, k)
		}
	}
	d, err := getDeployment(ctx, c, t)
	if err != nil {
		return Item{}, err
	}
	replicaSets, err := ownedReplicaSets(ctx, c, d)
	if err != nil {
		return Item{}, err
	}
	pods, err := ownedPods(ctx, c, d, replicaSets)
	if err != nil {
		return Item{}, err
	}
	pod, err := pickPod(pods, params["pod"])
	if err != nil {
		return Item{}, err
	}
	status, err := pickContainer(pod, params["container"])
	if err != nil {
		return Item{}, err
	}
	previous := status.LastTerminationState.Terminated != nil
	switch params["previous"] {
	case "":
	case "true":
		previous = true
	case "false":
		previous = false
	default:
		return Item{}, fmt.Errorf("%w: previous must be true or false", ErrInvalidParams)
	}

	req := LogRequest{
		Namespace: t.Namespace, Pod: pod.Name, Container: status.Name, Previous: previous,
		TailLines: logTailLines,
	}
	raw, read, err := l.read(ctx, req)
	if err != nil {
		return Item{}, err
	}
	gone := logsGone(raw, pod, status)
	var notes []string
	// Without a run asked for, the current one stands in for a gone one.
	if gone && previous && params["previous"] == "" {
		req.Previous, previous = false, false
		if raw, read, err = l.read(ctx, req); err != nil {
			return Item{}, err
		}
		if gone = logsGone(raw, pod, status); gone {
			notes = append(notes, goneNote(true)+", nor of the current run")
		} else {
			notes = append(notes, goneNote(true)+", so these are the current run's")
		}
	} else if gone {
		notes = append(notes, goneNote(previous))
	}
	if gone {
		raw, read = nil, 0
	}

	// The pod was picked by name: drop logs of another pod since recreated
	// under it.
	var now corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &now); err != nil {
		return Item{}, fmt.Errorf("recheck pod %s: %w", pod.Name, err)
	}
	if now.UID != pod.UID {
		return Item{}, fmt.Errorf("%w: pod %s was recreated", ErrTargetGone, pod.Name)
	}

	logs := tailUTF8(strings.ToValidUTF8(string(raw), "\uFFFD"), MaxLogBytes)
	if !gone && strings.TrimSpace(logs) == "" {
		notes = append(notes, emptyLogsNote)
	}
	view := LogsView{Pod: pod.Name, Container: status.Name, Previous: previous, Logs: logs, Note: strings.Join(notes, "; ")}
	data, err := json.Marshal(view)
	if err != nil {
		return Item{}, err
	}
	return Item{
		Data:           data,
		Truncated:      int64(len(logs)) < read,
		OriginalBytes:  int(read),
		Classification: ClassificationSensitive,
	}, nil
}

// read returns the end of a container's logs and how many bytes were read.
func (l containerLogsTail) read(ctx context.Context, req LogRequest) ([]byte, int64, error) {
	stream, err := l.logs.Logs(ctx, req)
	if err != nil {
		return nil, 0, fmt.Errorf("read logs of %s/%s: %w", req.Pod, req.Container, err)
	}
	defer func() { _ = stream.Close() }()
	var tail tailBuffer
	read, err := io.Copy(&tail, io.LimitReader(stream, maxLogReadBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("read logs of %s/%s: %w", req.Pod, req.Container, err)
	}
	return tail.buf, read, nil
}

// pickPod returns the named pod, which must be the target's, or the one
// needing attention most.
func pickPod(pods []*corev1.Pod, name string) (*corev1.Pod, error) {
	if name != "" {
		for _, p := range pods {
			if p.Name == name {
				return p, nil
			}
		}
		return nil, fmt.Errorf("%w: pod %q is not the target's", ErrInvalidParams, name)
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("%w: the target has no pods", ErrTargetGone)
	}
	return slices.MinFunc(pods, func(a, b *corev1.Pod) int {
		return cmp.Or(
			cmp.Compare(healthRank(viewPod(a, "")), healthRank(viewPod(b, ""))),
			cmp.Compare(a.Name, b.Name),
		)
	}), nil
}

// pickContainer returns the named container, or the one restarted most.
func pickContainer(pod *corev1.Pod, name string) (corev1.ContainerStatus, error) {
	statuses := pod.Status.ContainerStatuses
	if name != "" {
		for _, s := range append(slices.Clone(statuses), pod.Status.InitContainerStatuses...) {
			if s.Name == name {
				return s, nil
			}
		}
		return corev1.ContainerStatus{}, fmt.Errorf("%w: pod %s has no container %q", ErrInvalidParams, pod.Name, name)
	}
	if len(statuses) == 0 {
		if len(pod.Spec.Containers) == 0 {
			return corev1.ContainerStatus{}, fmt.Errorf("pod %s has no containers", pod.Name)
		}
		return corev1.ContainerStatus{Name: pod.Spec.Containers[0].Name}, nil
	}
	return slices.MaxFunc(statuses, func(a, b corev1.ContainerStatus) int {
		return cmp.Or(cmp.Compare(a.RestartCount, b.RestartCount), cmp.Compare(b.Name, a.Name))
	}), nil
}

// tailBuffer keeps the last 2*MaxLogBytes bytes written to it, enough to
// cut MaxLogBytes on a rune boundary after invalid bytes are replaced.
type tailBuffer struct {
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - 2*MaxLogBytes; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

// tailUTF8 keeps the last n bytes of s, starting on a rune boundary.
func tailUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
