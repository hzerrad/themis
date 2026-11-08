package governance_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability/probes"
	"github.com/hzerrad/themis/internal/catalog/capability/resources"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// literal is a secret written as a plain value in the workload's
// environment, as people do; scenario 1's own is fixture-s3cr3t-do-not-leak.
const literal = "hunter2"

// withLiteral sets DB_PASSWORD=hunter2 and adds a liveness probe in
// target's pod template, and in the ReplicaSet of its current revision, as
// the Deployment controller would.
func withLiteral(t *testing.T, target incident.TargetRef) {
	t.Helper()
	edit := func(tpl *corev1.PodTemplateSpec) {
		c := &tpl.Spec.Containers[0]
		for i := range c.Env {
			if c.Env[i].Name == "DB_PASSWORD" {
				c.Env[i].Value = literal
			}
		}
		c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}}, TimeoutSeconds: 1}
	}
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, &d); err != nil {
		t.Fatal(err)
	}
	edit(&d.Spec.Template)
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
	var rs appsv1.ReplicaSet
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: target.Namespace, Name: "checkout-7d9f8b6c4"}, &rs); err != nil {
		t.Fatal(err)
	}
	edit(&rs.Spec.Template)
	if err := k8s.Update(t.Context(), &rs); err != nil {
		t.Fatal(err)
	}
}

// holding lists the tables of Themis's database with a row that holds s,
// in any column.
func (e *env) holding(t *testing.T, s string) []string {
	t.Helper()
	rows, err := e.pool.Query(t.Context(), `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, table := range tables {
		if pgtest.Count(t, e.pool, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` r WHERE strpos(r::text, $1) > 0`, s) > 0 {
			out = append(out, table)
		}
	}
	return out
}

// No part of the workload's pod template reaches Themis's database, whatever
// the fix: a secret written as a plain value in its environment is in no
// row, whether the fix waits for approval, is only recommended, or is put
// back. People still see what the change does, the secret withheld.
func TestGovern_KeepsNoSecretOfTheTemplate(t *testing.T) {
	const (
		limit    = "the memory limit of container app changed from 256Mi to 64Mi"
		password = "the environment variable DB_PASSWORD of container app changed"
	)
	all := []string{rollback.Name, rollback.PartialName, resources.Name, probes.Name}
	p := profile("rollout-regression")
	p.Capabilities, p.JustifiedBy = all, map[string][]string{}
	for _, fix := range all {
		p.JustifiedBy[fix] = []string{"oom_killed_new_revision"}
	}
	for _, tt := range []struct {
		name   string
		fix    string
		params map[string]any
		reach  *governance.Reach
		want   incident.State
	}{
		{"a rollback", rollback.Name, map[string]any{"revision": int64(1)}, nil, incident.AwaitingApproval},
		{"a partial rollback", rollback.PartialName, map[string]any{"revision": int64(1), "changes": []any{limit, password}}, nil, incident.AwaitingApproval},
		{"a raise", resources.Name, map[string]any{"containers": map[string]any{"app": map[string]any{"limits": map[string]any{"memory": "128Mi"}}}}, nil, incident.AwaitingApproval},
		{"a probe change", probes.Name, map[string]any{"container": "app", "liveness": map[string]any{"timeoutSeconds": int64(3)}}, nil, incident.AwaitingApproval},
		{"a recommendation", rollback.PartialName, map[string]any{"revision": int64(1), "changes": []any{limit}}, &governance.Reach{}, incident.Recommended},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			withLiteral(t, e.target)
			e.worker.Config = config.Config{EnrolledNamespaces: []string{e.target.Namespace}, Profiles: []config.Profile{p}}
			e.worker.Reach = tt.reach
			id, proposal := e.suggested(t, "rollout-regression", predicates, tt.fix, tt.params)
			e.govern(t, proposal)

			if got := e.state(t, id); got != tt.want {
				t.Fatalf("state = %s (%q), want %s", got, e.lastReason(t, id), tt.want)
			}
			for _, secret := range []string{literal, "fixture-s3cr3t-do-not-leak"} {
				if held := e.holding(t, secret); len(held) > 0 {
					t.Errorf("%v hold %q", held, secret)
				}
			}
			a := e.action(t, id)
			if changes, err := catalog.Fixes()[a.Capability].Changes(a.Record); err != nil || len(changes) == 0 {
				t.Errorf("changes = %+v, %v; want what the fix changes", changes, err)
			}
		})
	}

	t.Run("a restore", func(t *testing.T) {
		e := shipped(t)
		withLiteral(t, e.target)
		id, fix := e.restoring(t)
		e.restore(t, fix)
		if actions := e.actions(t, id); len(actions) != 2 || actions[1].State != store.ActionAuthorized {
			t.Fatalf("%d actions, the last %s (%q); want the fix and its restore, authorized", len(actions), actions[len(actions)-1].State, e.lastReason(t, id))
		}
		if held := e.holding(t, literal); len(held) > 0 {
			t.Errorf("%v hold %q", held, literal)
		}
	})
}
