package rollback_test

import (
	"errors"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/rollback"
)

func TestSnapshot_TracksTheTemplate(t *testing.T) {
	target, d := load(t)
	before, err := rollback.Snapshot(t.Context(), k8s, target)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if before.SpecHash == "" || before.ChangedAt.IsZero() || time.Since(before.ChangedAt) > time.Minute {
		t.Errorf("snapshot = %+v", before)
	}

	d.Spec.Template.Annotations = map[string]string{"edited": "yes"}
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	after, err := rollback.Snapshot(t.Context(), k8s, target)
	if err != nil {
		t.Fatal(err)
	}
	if after.SpecHash == before.SpecHash {
		t.Error("a template change left the snapshot unchanged")
	}
}

func TestSnapshot_RecreatedTarget(t *testing.T) {
	target, _ := load(t)
	target.UID = "uid-of-a-deleted-deployment"
	if _, err := rollback.Snapshot(t.Context(), k8s, target); !errors.Is(err, rollback.ErrStale) {
		t.Errorf("err = %v, want ErrStale", err)
	}
}
