package capability_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/probes"
	"github.com/hzerrad/themis/internal/catalog/capability/resources"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/incident"
)

// A fix Themis only recommends says how to make its change with kubectl,
// from the record it keeps: exactly that change, each value quoted for a
// shell, and never a value that looks secret, or more of one than the
// change shows. When kubectl can't make it exactly, it says nothing.
func TestCommands(t *testing.T) {
	checkout := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "u1"}
	app := func(image, env, memory string) string {
		return `{"spec":{"containers":[{"name":"app","image":"` + image + `","env":[` + env + `],"resources":{"limits":{"memory":"` + memory + `"}}}]}}`
	}
	mode := func(v string) string { return `{"name":"MODE","value":"` + v + `"}` }
	// partial is a partial rollback's before and after, from the template it
	// replaces to the one it writes.
	partial := func(pre, desired string) [2]json.RawMessage {
		return [2]json.RawMessage{json.RawMessage(`{"template":` + pre + `,"earlier":` + desired + `}`), json.RawMessage(desired)}
	}
	for _, tt := range []struct {
		name      string
		fix       capability.Commanding
		params    map[string]any
		templates [2]json.RawMessage
		want      []string
	}{
		{
			name: "rollback", fix: rollback.Capability{}, params: map[string]any{"revision": int64(1)},
			want: []string{"kubectl -n shop rollout undo deployment/checkout --to-revision=1"},
		},
		{
			name: "image and environment put back", fix: rollback.Partial{},
			templates: partial(app("shop/checkout:1.5.0", mode("strict"), "48Mi"), app("shop/checkout:1.4.1", mode("healthy"), "48Mi")),
			want:      []string{"kubectl -n shop set image deployment/checkout app=shop/checkout:1.4.1", "kubectl -n shop set env deployment/checkout -c app MODE=healthy"},
		},
		{
			name: "a value with spaces, one removed and a memory limit", fix: rollback.Partial{},
			templates: partial(app("a:1", mode("strict")+`,{"name":"EXTRA","value":"1"}`, "48Mi"), app("a:1", mode("two words"), "64Mi")),
			want:      []string{"kubectl -n shop set env deployment/checkout -c app 'MODE=two words' EXTRA-", "kubectl -n shop set resources deployment/checkout -c app --limits=memory=64Mi"},
		},
		// kubectl adds a variable last: one put back before another can't be.
		{
			name: "a variable put back before another", fix: rollback.Partial{},
			templates: partial(app("a:1", mode("x"), "48Mi"), app("a:1", `{"name":"EXTRA","value":"1"},`+mode("x"), "48Mi")),
		},
		{
			name: "a value that looks secret", fix: rollback.Partial{},
			templates: partial(app("a:1", `{"name":"API_TOKEN","value":"old"}`, "48Mi"), app("a:1", `{"name":"API_TOKEN","value":"new"}`, "48Mi")),
		},
		{
			name: "a value longer than the change shows", fix: rollback.Partial{},
			templates: partial(app("a:1", mode("x"), "48Mi"), app("a:1", mode(strings.Repeat("long words ", 12)), "48Mi")),
		},
		{
			name: "a value from a Secret", fix: rollback.Partial{},
			templates: partial(app("a:1", mode("x"), "48Mi"), app("a:1", `{"name":"MODE","valueFrom":{"secretKeyRef":{"name":"s","key":"k"}}}`, "48Mi")),
		},
		{
			name: "a command changed", fix: rollback.Partial{},
			templates: partial(app("a:1", mode("x"), "48Mi"), `{"spec":{"containers":[{"name":"app","image":"a:1","command":["run"],"env":[`+mode("x")+`],"resources":{"limits":{"memory":"48Mi"}}}]}}`),
		},
		{
			name: "a memory limit raised", fix: resources.Capability{},
			templates: [2]json.RawMessage{json.RawMessage(app("a:1", "", "60Mi")), json.RawMessage(app("a:1", "", "72Mi"))},
			want:      []string{"kubectl -n shop set resources deployment/checkout -c app --limits=memory=72Mi"},
		},
		{
			name: "a probe lengthened", fix: probes.Capability{}, params: map[string]any{"container": "app", "liveness": map[string]any{"timeoutSeconds": int64(3)}},
			want: []string{`kubectl -n shop patch deployment/checkout -p '{"spec":{"template":{"spec":{"containers":[{"livenessProbe":{"timeoutSeconds":3},"name":"app"}]}}}}'`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var record json.RawMessage
			if tt.templates[0] != nil {
				var err error
				if record, err = tt.fix.(capability.Templating).Record(tt.params, tt.templates[0], tt.templates[1]); err != nil {
					t.Fatal(err)
				}
			}
			if got := tt.fix.Commands(checkout, tt.params, record); !slices.Equal(got, tt.want) {
				t.Errorf("commands = %q, want %q", got, tt.want)
			}
		})
	}
}
