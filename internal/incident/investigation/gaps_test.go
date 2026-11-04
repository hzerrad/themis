package investigation_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
	"github.com/hzerrad/themis/internal/workload"
)

// failing is a template whose every read fails with err.
type failing struct {
	name string
	err  error
}

func (f failing) Name() string      { return f.name }
func (failing) Description() string { return "" }
func (failing) Params() []string    { return nil }

func (f failing) Collect(context.Context, client.Reader, *workload.Workload, map[string]string) (evidence.Item, error) {
	return evidence.Item{}, f.err
}

// The gaps the diagnosis finds are the incident's: recorded once, before any
// lease, and logged, and the investigation goes on with what could be read.
// The alert's start is the incident's first alert's, so events past their
// retention are one of them.
func TestPrepare_RecordsTheDiagnosisGaps(t *testing.T) {
	e := newEnv(t)
	var logs bytes.Buffer
	e.worker.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	e.worker.Catalog.Evidence = evidence.NewRegistry(
		failing{evidence.RolloutHistory, apierrors.NewForbidden(schema.GroupResource{Resource: "replicasets"}, "", errors.New("RBAC: access denied"))},
		failing{evidence.PodStates, evidence.ErrNoPods},
		evidence.NewTargetEvents(),
	)
	id := e.newIncident(t, "rollout-regression", e.target)
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		return e.store.RecordFiringAlert(t.Context(), tx, store.FiringAlertArgs{
			IncidentID: id, Source: "alertmanager", SourceID: "fp-" + id, StartsAt: e.worker.Now().Add(-70 * time.Minute),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	// River delivers the job at least once: a second run records the gaps once.
	for range 2 {
		if err := e.worker.Work(t.Context(), &river.Job[investigation.InvestigateArgs]{Args: investigation.InvestigateArgs{IncidentID: id}}); err != nil {
			t.Fatalf("Work: %v", err)
		}
	}
	if got := e.state(t, id); got != incident.Investigating {
		t.Errorf("state = %s, want investigating", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM transitions WHERE incident_id = $1 AND reason = 'Investigation started with 1 piece of evidence.'`, id); n != 1 {
		t.Error("the timeline doesn't say the investigation started with the 1 piece that could be read")
	}
	gaps, err := e.store.EvidenceGaps(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		template string
		cause    evidence.GapCause
	}{
		{evidence.RolloutHistory, evidence.GapForbidden},
		{evidence.PodStates, evidence.GapAbsent},
		{evidence.TargetEvents, evidence.GapExpired},
	}
	if len(gaps) != len(want) {
		t.Fatalf("got %d gaps, want %d: %+v", len(gaps), len(want), gaps)
	}
	for i, g := range gaps {
		if g.Template != want[i].template || g.Cause != want[i].cause || g.LeaseID != "" || g.Detail == "" {
			t.Errorf("gap %d = %+v, want %s %s before any lease, with a detail", i, g, want[i].template, want[i].cause)
		}
		if !strings.Contains(logs.String(), "template="+g.Template) {
			t.Errorf("the %s gap isn't logged:\n%s", g.Template, logs.String())
		}
	}
}
