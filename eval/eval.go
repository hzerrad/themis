// Package eval builds what the evaluation replays: each scenario pack's
// cases, a cluster each, read by the real evidence templates and the pack's
// checks, written as a bundle a model can be run against with no cluster. It
// also checks that a pack is complete.
package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/workload"
)

// Decisions a case can expect of an investigation.
const (
	DecisionPropose = "propose"
	DecisionAbstain = "abstain"
	DecisionEither  = "either"
)

// Case is what an evaluation case's case.yaml declares. Changing Truth means
// a new Version, so results are never compared across different answers.
type Case struct {
	Name          string   `json:"name"`
	Version       int      `json:"version"`
	Description   string   `json:"description"`
	Tags          []string `json:"tags,omitempty"`
	Source        string   `json:"source"`
	Difficulty    string   `json:"difficulty"`
	IncidentClass string   `json:"incident_class"`

	// Now is when the investigation happens, since checks depend on the time.
	Now    time.Time `json:"now"`
	Target Target    `json:"target"`

	Evidence Evidence `json:"evidence"`
	Truth    Truth    `json:"truth"`

	KnownLimitations string `json:"known_limitations,omitempty"`
}

// Target names the incident's workload in cluster.yaml.
type Target struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Evidence says what the investigation starts with and may ask for.
type Evidence struct {
	Initial []string `json:"initial"`
	Allowed []string `json:"allowed"`
}

// Truth is the right answer.
type Truth struct {
	// RootCauses are the classes accepted as correct.
	RootCauses []string `json:"root_causes"`
	Decision   string   `json:"decision"`

	// AcceptableRevisions are the rollbacks that would be right.
	AcceptableRevisions []int64 `json:"acceptable_revisions,omitempty"`
	// RollbackIsHarmful marks any rollback as harmful, not merely wrong.
	RollbackIsHarmful bool `json:"rollback_is_harmful,omitempty"`
	// RequiredCitations are templates a correct proposal must cite.
	RequiredCitations []string `json:"required_citations,omitempty"`

	// MustNotSay is text an injected instruction asks for: writing it means
	// the model obeyed the evidence.
	MustNotSay []string `json:"must_not_say,omitempty"`

	// Predicates are the checks' right answers. Those not listed aren't
	// compared.
	Predicates map[string]string `json:"predicates"`
}

// Validate reports every problem in s, not only the first. rootCauses are the
// root causes its pack declares.
func (s Case) Validate(rootCauses []string) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if s.Name == "" {
		add("name is required")
	}
	if s.Version < 1 {
		add("version must be at least 1")
	}
	if !slices.Contains([]string{"synthetic", "adapted", "community"}, s.Source) {
		add("source must be synthetic, adapted or community, got %q", s.Source)
	}
	if !slices.Contains([]string{"easy", "medium", "hard"}, s.Difficulty) {
		add("difficulty must be easy, medium or hard, got %q", s.Difficulty)
	}
	if s.Now.IsZero() {
		add("now is required")
	}
	if s.Target.Kind != "Deployment" || s.Target.Namespace == "" || s.Target.Name == "" {
		add("target must name a Deployment and its namespace")
	}
	if len(s.Evidence.Allowed) == 0 {
		add("evidence.allowed needs a template")
	}
	for _, name := range s.Evidence.Initial {
		if !slices.Contains(s.Evidence.Allowed, name) {
			add("evidence.initial: %s is not in evidence.allowed", name)
		}
	}
	if len(s.Truth.RootCauses) == 0 {
		add("truth.root_causes needs a class")
	}
	for _, c := range s.Truth.RootCauses {
		if !slices.Contains(rootCauses, c) {
			add("truth.root_causes: %s isn't one of the root causes in the pack's scenario.yaml", c)
		}
	}
	switch s.Truth.Decision {
	case DecisionPropose:
		if len(s.Truth.AcceptableRevisions) == 0 {
			add("truth.acceptable_revisions is required when the right decision is to propose")
		}
		if s.Truth.RollbackIsHarmful {
			add("truth: a rollback can't be both right and harmful")
		}
	case DecisionAbstain, DecisionEither:
	default:
		add("truth.decision must be propose, abstain or either, got %q", s.Truth.Decision)
	}
	return errors.Join(errs...)
}

// Load reads and validates the case in dir, of a pack declaring rootCauses.
// Unknown keys are errors.
func Load(dir string, rootCauses []string) (Case, error) {
	data, err := os.ReadFile(filepath.Join(dir, "case.yaml")) //nolint:gosec // a path in the repository
	if err != nil {
		return Case{}, err
	}
	var s Case
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return Case{}, fmt.Errorf("case.yaml: %w", err)
	}
	if err := s.Validate(rootCauses); err != nil {
		return Case{}, prefixed("case.yaml", err)
	}
	if s.Name != filepath.Base(dir) {
		return Case{}, fmt.Errorf("case.yaml: name %q must be the folder's, %q", s.Name, filepath.Base(dir))
	}
	return s, nil
}

// Bundle is what a replay needs, with nothing random in it, so a stale one
// shows as a changed file.
type Bundle struct {
	Scenario      string `json:"scenario"`
	Version       int    `json:"version"`
	Difficulty    string `json:"difficulty"`
	IncidentClass string `json:"incident_class"`

	// Truth is for the scorer: the replay never shows it to the model.
	Truth Truth `json:"truth"`

	Target incident.TargetRef `json:"target"`

	Capabilities []string `json:"capabilities"`
	// Fixes describe the capabilities as the core sends them to the
	// investigator, so the replay sees the same prompt.
	Fixes []BundleFix `json:"fixes"`
	// RootCauses are the root causes the core sends the investigator for
	// the pack's profile, for the same reason.
	RootCauses          []catalog.RootCause `json:"root_causes"`
	MaxSteps            int                 `json:"max_steps"`
	MaxEvidenceRequests int                 `json:"max_evidence_requests"`

	// Initial is what the investigation starts with; Requestable is what it
	// may ask for, by template.
	Initial     []Item          `json:"initial"`
	Requestable map[string]Item `json:"requestable"`
	Templates   []Template      `json:"templates"`
	// Gaps are the initial evidence the core couldn't read.
	Gaps []Gap `json:"gaps,omitempty"`

	Predicates []Check `json:"predicates"`

	// Policy is what governance decides for a rollback of the target to the
	// revision before the current one, given these checks. A harmful proposal the policy denies never reaches
	// anyone; one it lets through reaches a person to approve.
	Policy PolicyCheck `json:"policy"`
}

// PolicyCheck is a policy's verdict on rolling the target back.
type PolicyCheck struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

// Template is an evidence template, described as the core describes it to an
// investigator.
type Template struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Params      []string `json:"params,omitempty"`
}

// Item is one piece of evidence, redacted as the core would send it.
type Item struct {
	ID             string `json:"id"`
	Template       string `json:"template"`
	Data           string `json:"data"`
	Truncated      bool   `json:"truncated,omitempty"`
	OriginalBytes  int    `json:"original_bytes,omitempty"`
	Redactions     int    `json:"redactions,omitempty"`
	Classification string `json:"classification"`
}

// Check is one check's answer. An unknown one carries the gaps in the
// evidence it reads, and is blind when Themis couldn't see.
type Check struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Value       string   `json:"value"`
	Reason      string   `json:"reason"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	Gaps        []Gap    `json:"gaps,omitempty"`
	Blind       bool     `json:"blind,omitempty"`
}

// Gap is initial evidence the core couldn't read, and why.
type Gap struct {
	Template string `json:"template"`
	Cause    string `json:"cause"`
	Detail   string `json:"detail,omitempty"`
}

func bundleItem(item evidence.Item) Item {
	return Item{
		ID: item.ID, Template: item.Template, Data: string(item.Data), Truncated: item.Truncated,
		OriginalBytes: item.OriginalBytes, Redactions: item.Redactions, Classification: string(item.Classification),
	}
}

// Build reads the case in dir with the real templates and the checks of
// profile, a scenario pack's from packs, and decides a rollback as governance
// would.
func Build(ctx context.Context, dir string, s Case, packs fs.FS, profile config.Profile) (Bundle, error) {
	c, target, err := loadCluster(filepath.Join(dir, "cluster.yaml"), s.Target)
	if err != nil {
		return Bundle{}, err
	}
	cat, err := catalog.Build(fileLogs{dir: filepath.Join(dir, "logs")}, nil, packs, nil)
	if err != nil {
		return Bundle{}, err
	}

	b := Bundle{
		Scenario: s.Name, Version: s.Version, Difficulty: s.Difficulty, IncidentClass: s.IncidentClass,
		Truth: s.Truth, Target: target, Capabilities: profile.Capabilities, Fixes: bundleFixes(cat.Fixes, profile.Capabilities),
		RootCauses: cat.RootCauses.For(profile.Name), MaxSteps: config.DefaultMaxSteps, MaxEvidenceRequests: config.DefaultMaxEvidenceRequests,
		Requestable: map[string]Item{},
	}
	w, err := workload.Read(ctx, c, target)
	if err != nil {
		return Bundle{}, err
	}
	// Diagnosed as the core diagnoses an incident, under the profile with the
	// case's evidence. A case has no alert, so no events have expired.
	p := profile
	p.Evidence = config.Evidence{Initial: s.Evidence.Initial, Allowed: s.Evidence.Allowed}
	d := cat.Diagnose(ctx, c, w, p, catalog.Moment{Now: s.Now})
	// Items are named after their template, so a rebuilt bundle is the same.
	ids := map[string]string{}
	for _, item := range d.Items {
		ids[item.ID] = "ev-" + item.Template
		item.ID = ids[item.ID]
		b.Initial = append(b.Initial, bundleItem(item))
	}
	for _, g := range d.Gaps {
		b.Gaps = append(b.Gaps, Gap{Template: g.Template, Cause: string(g.Cause), Detail: g.Detail})
	}
	for _, name := range s.Evidence.Allowed {
		about := cat.Evidence[name]
		b.Templates = append(b.Templates, Template{Name: name, Description: about.Description(), Params: about.Params()})
		if slices.Contains(s.Evidence.Initial, name) {
			continue
		}
		// A replay can only hand over an item, so a template that can't be
		// read stops the build.
		item, err := cat.Evidence.Collect(ctx, c, name, w, nil, s.Now)
		if err != nil {
			return Bundle{}, fmt.Errorf("%s: %w", name, err)
		}
		item.ID = "ev-" + name
		b.Requestable[name] = bundleItem(item)
	}
	for _, r := range d.Results {
		out := Check{Name: r.Name, Version: r.Version, Value: r.Value.String(), Reason: r.Reason, Blind: r.Blind}
		for _, id := range r.EvidenceIDs {
			out.EvidenceIDs = append(out.EvidenceIDs, ids[id])
		}
		for _, g := range r.Gaps {
			out.Gaps = append(out.Gaps, Gap{Template: g.Template, Cause: string(g.Cause)})
		}
		b.Predicates = append(b.Predicates, out)
	}
	b.Policy, err = decide(ctx, c, w, profile, d.Results, cat.Fixes[rollback.Name])
	if err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// decide decides, as governance decides the investigator's suggestion, a
// rollback of w to the revision before its current one. A rollback that
// can't go ahead reaches nobody, as a denied one doesn't.
func decide(ctx context.Context, c client.Reader, w *workload.Workload, profile config.Profile, checks []check.Result, fix capability.Capability) (PolicyCheck, error) {
	revisions, err := w.Revisions(ctx)
	if err != nil {
		return PolicyCheck{}, err
	}
	var previous int64
	for _, r := range revisions {
		if r.Number < w.Revision && r.Number > previous {
			previous = r.Number
		}
	}
	if previous == 0 {
		return PolicyCheck{Outcome: string(policy.Deny), Reason: "The workload has no earlier revision to roll back to."}, nil
	}
	var decider governance.Decider
	decided, err := decider.Decide(ctx, c, governance.Fix{
		Target: w.Target, Profile: profile, Checks: checks,
		Capability: fix, Params: map[string]any{"revision": previous},
		By: governance.Suggester{Source: "themis-intelligence", Authenticated: true},
	})
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		return PolicyCheck{Outcome: string(policy.Deny), Reason: refusal.Reason}, nil
	}
	if err != nil {
		return PolicyCheck{}, err
	}
	return PolicyCheck{Outcome: string(decided.Decision.Outcome), Reason: decided.Decision.Reason}, nil
}

// loadCluster reads a cluster the way the controllers and kubelet left it.
// Owners, and the objects events are about, are linked by kind and name, so
// fixtures carry no IDs and every build gets the same ones.
func loadCluster(path string, want Target) (client.Reader, incident.TargetRef, error) {
	f, err := os.Open(path) //nolint:gosec // a path in the repository
	if err != nil {
		return nil, incident.TargetRef{}, err
	}
	defer func() { _ = f.Close() }()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, incident.TargetRef{}, err
	}
	// Checks read when objects were last written from their managed fields.
	builder := fake.NewClientBuilder().WithScheme(scheme).WithReturnManagedFields()
	target := incident.TargetRef{}
	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, incident.TargetRef{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if len(u.Object) == 0 {
			continue
		}
		u.SetNamespace(want.Namespace)
		u.SetUID(uid(u.GetKind(), u.GetName()))
		refs := u.GetOwnerReferences()
		for i, ref := range refs {
			refs[i].UID = uid(ref.Kind, ref.Name)
		}
		u.SetOwnerReferences(refs)
		if u.GetKind() == "Event" {
			kind, _, _ := unstructured.NestedString(u.Object, "involvedObject", "kind")
			name, _, _ := unstructured.NestedString(u.Object, "involvedObject", "name")
			if err := unstructured.SetNestedField(u.Object, string(uid(kind, name)), "involvedObject", "uid"); err != nil {
				return nil, incident.TargetRef{}, err
			}
		}
		typed, err := scheme.New(u.GroupVersionKind())
		if err != nil {
			return nil, incident.TargetRef{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, typed); err != nil {
			return nil, incident.TargetRef{}, fmt.Errorf("%s %s: %w", u.GetKind(), u.GetName(), err)
		}
		obj, ok := typed.(client.Object)
		if !ok {
			return nil, incident.TargetRef{}, fmt.Errorf("%s isn't an object", u.GetKind())
		}
		builder = builder.WithObjects(obj).WithStatusSubresource(obj)
		if u.GetKind() == want.Kind && u.GetName() == want.Name {
			target = incident.TargetRef{Group: u.GroupVersionKind().Group, Kind: want.Kind, Namespace: want.Namespace, Name: want.Name, UID: string(u.GetUID())}
		}
	}
	if target.UID == "" {
		return nil, incident.TargetRef{}, fmt.Errorf("%s has no %s %s, the case's target", filepath.Base(path), want.Kind, want.Name)
	}
	return builder.Build(), target, nil
}

func uid(kind, name string) types.UID {
	return types.UID("uid-" + strings.ToLower(kind) + "-" + name)
}

// fileLogs reads logs/<pod>.txt. A pod with no file wrote nothing.
type fileLogs struct {
	dir string
}

func (l fileLogs) Logs(_ context.Context, req evidence.LogRequest) (io.ReadCloser, error) {
	data, err := os.ReadFile(filepath.Join(l.dir, req.Pod+".txt")) //nolint:gosec // a path in the repository
	if errors.Is(err, os.ErrNotExist) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// BundleFix is a fix as the core describes it to the investigator.
type BundleFix struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ParamsSchema string `json:"params_schema"`
}

// bundleFixes describes the fixes the profile allows: the investigator is
// offered those and no others, so a fix added to Themis changes no pack's
// bundles until its profile allows it.
func bundleFixes(fixes capability.Registry, allowed []string) []BundleFix {
	var out []BundleFix
	for _, name := range allowed {
		f, ok := fixes[name]
		if !ok {
			continue
		}
		out = append(out, BundleFix{Name: f.Name(), Description: f.Description(), ParamsSchema: string(f.Schema())})
	}
	return out
}
