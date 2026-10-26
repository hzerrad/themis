package intake_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/intake"
	"github.com/hzerrad/themis/internal/investigation"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// The worker tests run a real River client, so the job's completion in the
// worker's transaction is exercised as in production.

type noopInvestigate struct {
	river.WorkerDefaults[investigation.InvestigateArgs]
}

func (noopInvestigate) Work(context.Context, *river.Job[investigation.InvestigateArgs]) error {
	return nil
}

// failingReader stands in for the API server, failing every read with err.
type failingReader struct{ err error }

func (r failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func (r failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.err
}

type workerEnv struct {
	store  *store.Store
	pool   *pgxpool.Pool
	client *river.Client[pgx.Tx]
	events <-chan *river.Event
	ns     string
}

func profileConfig(ns string) config.Config {
	return config.Config{
		EnrolledNamespaces: []string{ns},
		Profiles: []config.Profile{{
			Name:     "rollout-regression",
			Priority: 100,
			Match: config.Match{
				AlertNames:  []string{"KubePodCrashLooping", "KubePodOOMKilled"},
				TargetKinds: []string{"Deployment.apps"},
			},
		}},
	}
}

// newWorkerEnv starts a River client working process_alert jobs with reader
// standing in for the cluster, or the envtest API server when reader is nil.
func newWorkerEnv(t *testing.T, reader client.Reader) *workerEnv {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	ns := newNamespace(t)
	if reader == nil {
		reader = k8s
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, &intake.ProcessAlertWorker{
		Config: profileConfig(ns),
		Store:  s,
		Reader: reader,
		Mapper: k8s.RESTMapper(),
	})
	river.AddWorker(workers, noopInvestigate{})

	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:           workers,
		FetchPollInterval: 50 * time.Millisecond,
		FetchCooldown:     10 * time.Millisecond,
		Logger:            slog.New(slog.DiscardHandler),
		TestOnly:          true,
	})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	events, unsubscribe := c.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start river: %v", err)
	}
	t.Cleanup(func() {
		unsubscribe()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Stop(ctx)
	})
	return &workerEnv{store: s, pool: pool, client: c, events: events, ns: ns}
}

// process queues the alert and waits for its job's first outcome.
func (e *workerEnv) process(t *testing.T, a intake.Alert, opts *river.InsertOpts) *river.Event {
	t.Helper()
	res, err := e.client.Insert(t.Context(), intake.ProcessAlertArgs{Alert: a}, opts)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev := <-e.events:
			if ev.Job.ID == res.Job.ID {
				return ev
			}
		case <-timeout:
			t.Fatalf("job %d did not finish", res.Job.ID)
		}
	}
}

func wantCompleted(t *testing.T, ev *river.Event) {
	t.Helper()
	if ev.Kind != river.EventKindJobCompleted {
		t.Fatalf("job event = %s (state %s, errors %v), want completed", ev.Kind, ev.Job.State, ev.Job.Errors)
	}
}

func alertFor(ns, pod, sourceID string, status intake.AlertStatus) intake.Alert {
	a := intake.Alert{
		Source:     intake.AlertmanagerSource,
		SourceID:   sourceID,
		Name:       "KubePodCrashLooping",
		Status:     status,
		Labels:     map[string]string{"alertname": "KubePodCrashLooping", "namespace": ns, "pod": pod},
		StartsAt:   time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
		ReceivedAt: receivedAt,
		Payload:    json.RawMessage(fmt.Sprintf(`{"labels":{"pod":%q}}`, pod)),
	}
	if status == intake.AlertResolved {
		a.EndsAt = a.StartsAt.Add(20 * time.Minute)
	}
	return a
}

func (e *workerEnv) rejectionReason(t *testing.T) string {
	t.Helper()
	var reason string
	if err := e.pool.QueryRow(t.Context(), `SELECT reason::text FROM rejected_alerts`).Scan(&reason); err != nil {
		t.Fatalf("read rejection: %v", err)
	}
	return reason
}

func (e *workerEnv) onlyIncident(t *testing.T) (id string, state incident.State, occurrences int) {
	t.Helper()
	err := e.pool.QueryRow(t.Context(), `SELECT id::text, state::text, occurrence_count FROM incidents`).
		Scan(&id, &state, &occurrences)
	if err != nil {
		t.Fatalf("read incident: %v", err)
	}
	return id, state, occurrences
}

func TestWorker_FiringAlertOpensIncident(t *testing.T) {
	e := newWorkerEnv(t, nil)
	dep, pod := newRollout(t, e.ns, "checkout")

	wantCompleted(t, e.process(t, alertFor(e.ns, pod.Name, "fp-a", intake.AlertFiring), nil))

	var profile, uid string
	var state incident.State
	err := e.pool.QueryRow(t.Context(), `SELECT profile, target_uid, state::text FROM incidents`).Scan(&profile, &uid, &state)
	if err != nil {
		t.Fatalf("read incident: %v", err)
	}
	if profile != "rollout-regression" || uid != string(dep.UID) || state != incident.Received {
		t.Errorf("incident = %s %s %s, want rollout-regression, the Deployment's UID, received", profile, uid, state)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM incident_alerts WHERE source_id = 'fp-a' AND status = 'firing'`); n != 1 {
		t.Errorf("firing incident alerts = %d, want 1", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = 'investigate'`); n != 1 {
		t.Errorf("investigate jobs = %d, want 1", n)
	}
}

// Two pods of one Deployment are one problem: one incident, one
// investigation, two alerts.
func TestWorker_SecondAlertJoins(t *testing.T) {
	e := newWorkerEnv(t, nil)
	_, pod1 := newRollout(t, e.ns, "checkout")
	pod2 := newPod(t, e.ns, "checkout-2", pod1.OwnerReferences[0])

	wantCompleted(t, e.process(t, alertFor(e.ns, pod1.Name, "fp-a", intake.AlertFiring), nil))
	wantCompleted(t, e.process(t, alertFor(e.ns, pod2.Name, "fp-b", intake.AlertFiring), nil))

	if _, _, occurrences := e.onlyIncident(t); occurrences != 2 {
		t.Errorf("occurrence_count = %d, want 2", occurrences)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM incident_alerts`); n != 2 {
		t.Errorf("incident alerts = %d, want 2", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = 'investigate'`); n != 1 {
		t.Errorf("investigate jobs = %d, want 1", n)
	}
}

func TestWorker_Rejections(t *testing.T) {
	tests := []struct {
		name   string
		reader client.Reader
		alert  func(t *testing.T, e *workerEnv) intake.Alert
		want   string
	}{
		{
			name: "namespace not enrolled",
			alert: func(t *testing.T, e *workerEnv) intake.Alert {
				return alertFor("kube-system", "coredns-1", "fp", intake.AlertFiring)
			},
			want: "not_enrolled",
		},
		{
			name: "target not found",
			alert: func(t *testing.T, e *workerEnv) intake.Alert {
				return alertFor(e.ns, "no-such-pod", "fp", intake.AlertFiring)
			},
			want: "target_not_found",
		},
		{
			name: "missing labels",
			alert: func(t *testing.T, e *workerEnv) intake.Alert {
				a := alertFor(e.ns, "x", "fp", intake.AlertFiring)
				delete(a.Labels, "pod")
				return a
			},
			want: "missing_labels",
		},
		{
			name: "no profile",
			alert: func(t *testing.T, e *workerEnv) intake.Alert {
				_, pod := newRollout(t, e.ns, "checkout")
				a := alertFor(e.ns, pod.Name, "fp", intake.AlertFiring)
				a.Name = "KubeDeploymentReplicasMismatch"
				return a
			},
			want: "no_profile",
		},
		{
			name:   "forbidden",
			reader: failingReader{apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("no RBAC"))},
			alert: func(t *testing.T, e *workerEnv) intake.Alert {
				return alertFor(e.ns, "x", "fp", intake.AlertFiring)
			},
			want: "forbidden",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newWorkerEnv(t, tt.reader)

			wantCompleted(t, e.process(t, tt.alert(t, e), nil))

			if got := e.rejectionReason(t); got != tt.want {
				t.Errorf("reason = %s, want %s", got, tt.want)
			}
			if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM incidents`); n != 0 {
				t.Errorf("incidents = %d, want 0", n)
			}
		})
	}
}

// A transient error is retried, not rejected, while attempts remain.
func TestWorker_TransientErrorRetried(t *testing.T) {
	e := newWorkerEnv(t, failingReader{errors.New("connection refused")})

	ev := e.process(t, alertFor(e.ns, "x", "fp", intake.AlertFiring), &river.InsertOpts{MaxAttempts: 2})

	// After a short backoff River may already have made the job available
	// again; either way it is due to run, not given up.
	if ev.Kind != river.EventKindJobFailed ||
		(ev.Job.State != rivertype.JobStateRetryable && ev.Job.State != rivertype.JobStateAvailable) {
		t.Errorf("job event = %s (state %s), want failed and due to retry", ev.Kind, ev.Job.State)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM rejected_alerts`); n != 0 {
		t.Errorf("rejections = %d, want 0", n)
	}
}

// On the last attempt, a transient error becomes a recorded rejection.
func TestWorker_LastAttemptRecordsProcessingFailed(t *testing.T) {
	e := newWorkerEnv(t, failingReader{errors.New("connection refused")})

	wantCompleted(t, e.process(t, alertFor(e.ns, "x", "fp", intake.AlertFiring), &river.InsertOpts{MaxAttempts: 1}))

	if got := e.rejectionReason(t); got != "processing_failed" {
		t.Errorf("reason = %s, want processing_failed", got)
	}
}

// The incident closes only when its last alert resolves.
func TestWorker_SelfResolvesWhenAllAlertsResolve(t *testing.T) {
	e := newWorkerEnv(t, nil)
	_, pod1 := newRollout(t, e.ns, "checkout")
	pod2 := newPod(t, e.ns, "checkout-2", pod1.OwnerReferences[0])
	wantCompleted(t, e.process(t, alertFor(e.ns, pod1.Name, "fp-a", intake.AlertFiring), nil))
	wantCompleted(t, e.process(t, alertFor(e.ns, pod2.Name, "fp-b", intake.AlertFiring), nil))

	wantCompleted(t, e.process(t, alertFor(e.ns, pod1.Name, "fp-a", intake.AlertResolved), nil))
	if _, state, _ := e.onlyIncident(t); state != incident.Received {
		t.Fatalf("state after one of two alerts resolved = %s, want received", state)
	}

	wantCompleted(t, e.process(t, alertFor(e.ns, pod2.Name, "fp-b", intake.AlertResolved), nil))
	if _, state, _ := e.onlyIncident(t); state != incident.SelfResolved {
		t.Errorf("state after both alerts resolved = %s, want self_resolved", state)
	}
}

// Verification, not the alert, decides the outcome of a fix in flight.
func TestWorker_ResolvedDoesNotCloseActionInFlight(t *testing.T) {
	e := newWorkerEnv(t, nil)
	_, pod := newRollout(t, e.ns, "checkout")
	wantCompleted(t, e.process(t, alertFor(e.ns, pod.Name, "fp-a", intake.AlertFiring), nil))
	id, _, _ := e.onlyIncident(t)
	path := []incident.State{incident.Received, incident.Investigating, incident.Diagnosed,
		incident.Proposed, incident.Authorized, incident.Executing}
	for i := 1; i < len(path); i++ {
		err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
			return e.store.Transition(t.Context(), tx, store.TransitionArgs{
				ID: id, From: path[i-1], To: path[i], Reason: "test", Actor: "test",
			})
		})
		if err != nil {
			t.Fatalf("Transition to %s: %v", path[i], err)
		}
	}

	wantCompleted(t, e.process(t, alertFor(e.ns, pod.Name, "fp-a", intake.AlertResolved), nil))

	if _, state, _ := e.onlyIncident(t); state != incident.Executing {
		t.Errorf("state = %s, want executing", state)
	}
}

// A resolved alert is handled without reading the cluster: its target may be
// gone.
func TestWorker_ResolvedNeedsNoCluster(t *testing.T) {
	e := newWorkerEnv(t, failingReader{errors.New("connection refused")})

	wantCompleted(t, e.process(t, alertFor(e.ns, "gone", "fp-unknown", intake.AlertResolved), nil))

	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM rejected_alerts`); n != 0 {
		t.Errorf("rejections = %d, want 0: a resolved alert without an incident is not a rejection", n)
	}
}
