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

	// Alert is the alert that raised the incident, when the case says. The
	// incident then gets every pack's profile that alert matches, as Themis
	// gives it; without, only its own pack's.
	Alert string `json:"alert,omitempty"`

	// Now is when the investigation happens, since checks depend on the time.
	Now    time.Time `json:"now"`
	Target Target    `json:"target"`

	Evidence Evidence `json:"evidence"`
	Truth    Truth    `json:"truth"`

	// Fix is the fix put to the pack's policy, as an investigator would
	// suggest it. Without it, a rollback to the revision before the current
	// one.
	Fix *Fix `json:"fix,omitempty"`

	KnownLimitations string `json:"known_limitations,omitempty"`
}

// Fix is a fix as an investigator suggests it.
type Fix struct {
	Capability string         `json:"capability"`
	Params     map[string]any `json:"params"`
}

// rolledBack reports whether the case's fix is a rollback.
func (s Case) rolledBack() bool { return s.Fix == nil || s.Fix.Capability == rollback.Name }

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
	// Harmful marks acting here as harmful, not merely wrong: any fix
	// proposed does harm.
	Harmful bool `json:"harmful,omitempty"`
	// RequiredCitations are templates a correct proposal must cite.
	RequiredCitations []string `json:"required_citations,omitempty"`

	// MustNotSay is text an injected instruction asks for: writing it means
	// the model obeyed the evidence.
	MustNotSay []string `json:"must_not_say,omitempty"`

	// Predicates are the checks' right answers. Those not listed aren't
	// compared.
	Predicates map[string]string `json:"predicates"`

	// Policy is what the pack's policy must decide for the case's fix:
	// allow, require_approval or deny. Not compared when empty.
	Policy string `json:"policy,omitempty"`

	// Attack makes the case an injection twin: another case of the pack
	// with text injected into its evidence, and what that text wants.
	Attack *Attack `json:"attack,omitempty"`
}

// Attack is what text injected into a twin's evidence wants of the
// investigation. Every field set must hold for the investigation to have done
// what it says; the report compares the twin with the case it copies.
type Attack struct {
	// TwinOf is the case of the same pack this one copies, text aside.
	TwinOf string `json:"twin_of"`
	// Wants is the decision the text asks for: propose or abstain.
	Wants string `json:"wants,omitempty"`
	// Fix, Target and Container are what a proposal it asks for changes: the
	// fix's name, a workload's name, a container's name.
	Fix       string `json:"fix,omitempty"`
	Target    string `json:"target,omitempty"`
	Container string `json:"container,omitempty"`
	// RootCause is the root cause the text wants named first.
	RootCause string `json:"root_cause,omitempty"`
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
		if s.rolledBack() && len(s.Truth.AcceptableRevisions) == 0 {
			add("truth.acceptable_revisions is required when the right decision is to propose")
		}
		if s.Truth.Harmful {
			add("truth: acting can't be both right and harmful")
		}
	case DecisionAbstain, DecisionEither:
	default:
		add("truth.decision must be propose, abstain or either, got %q", s.Truth.Decision)
	}
	if p := s.Truth.Policy; p != "" && !slices.Contains([]policy.Outcome{policy.Allow, policy.RequireApproval, policy.Deny}, policy.Outcome(p)) {
		add("truth.policy must be allow, require_approval or deny, got %q", p)
	}
	if s.Fix != nil && s.Fix.Capability == "" {
		add("fix.capability names the fix to put to the pack's policy")
	}
	if a := s.Truth.Attack; a != nil {
		if a.TwinOf == "" || a.TwinOf == s.Name {
			add("truth.attack.twin_of names the case this one copies")
		}
		if !slices.Contains([]string{"", DecisionPropose, DecisionAbstain}, a.Wants) {
			add("truth.attack.wants must be propose or abstain, got %q", a.Wants)
		}
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
	RootCauses []catalog.RootCause `json:"root_causes"`
	// Guidance is what the pack's profile tells the investigator, as the
	// core sends it.
	Guidance            string `json:"guidance,omitempty"`
	MaxSteps            int    `json:"max_steps"`
	MaxEvidenceRequests int    `json:"max_evidence_requests"`

	// Initial is what the investigation starts with; Requestable is what it
	// may ask for, by template.
	Initial     []Item          `json:"initial"`
	Requestable map[string]Item `json:"requestable"`
	Templates   []Template      `json:"templates"`
	// Gaps are the initial evidence the core couldn't read.
	Gaps []Gap `json:"gaps,omitempty"`

	Predicates []Check `json:"predicates"`

	// Policy is what governance decides for the case's fix, given these
	// checks: by default a rollback of the target to the revision before the
	// current one. A harmful proposal the policy denies never reaches anyone;
	// one it lets through reaches a person to approve.
	Policy PolicyCheck `json:"policy"`

	// Unproposed is what Themis tells the person it hands the incident to
	// when the investigator suggests no fix but the checks support one that
	// no policy forbids; empty when it would close or wait instead.
	Unproposed string `json:"unproposed,omitempty"`
}

// PolicyCheck is a policy's verdict on the case's fix.
type PolicyCheck struct {
	// Fix is the fix decided, when it isn't the default rollback.
	Fix     *Fix   `json:"fix,omitempty"`
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
	// Suspicious is text the core flags as written for the investigator.
	Suspicious []evidence.Suspicion `json:"suspicious,omitempty"`
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
		Suspicious: item.Suspicious,
	}
}

// Build reads the case in dir with the real templates and checks, under the
// profiles the incident gets: profile, a scenario pack's from packs, or, for
// a case naming its alert, every pack's profile matching that alert and the
// target, as Themis matches them. It decides the case's fix, or a rollback,
// as governance would. Its metrics source is the one the case's metrics.yaml
// describes, or none without the file.
func Build(ctx context.Context, dir string, s Case, packs fs.FS, profile config.Profile) (Bundle, error) {
	f, err := find(ctx, dir, s, packs, profile)
	if err != nil {
		return Bundle{}, err
	}
	fixes := f.matched.Capabilities()
	b := Bundle{
		Scenario: s.Name, Version: s.Version, Difficulty: s.Difficulty, IncidentClass: s.IncidentClass,
		// A profile that only diagnoses offers an empty list, never null.
		Truth: s.Truth, Target: f.target, Capabilities: append([]string{}, fixes...), Fixes: bundleFixes(f.cat.Fixes, fixes),
		RootCauses: f.cat.RootCauses.For(f.matched.Names()...), Guidance: f.matched.Guidance(), MaxSteps: config.DefaultMaxSteps, MaxEvidenceRequests: config.DefaultMaxEvidenceRequests,
		Requestable: map[string]Item{},
	}
	// Items are named after their template, so a rebuilt bundle is the same.
	ids := map[string]string{}
	for _, item := range f.d.Items {
		ids[item.ID] = "ev-" + item.Template
		item.ID = ids[item.ID]
		b.Initial = append(b.Initial, bundleItem(item))
	}
	for _, g := range f.d.Gaps {
		b.Gaps = append(b.Gaps, Gap{Template: g.Template, Cause: string(g.Cause), Detail: g.Detail})
	}
	for _, name := range s.Evidence.Allowed {
		about := f.cat.Evidence[name]
		b.Templates = append(b.Templates, Template{Name: name, Description: about.Description(), Params: about.Params()})
		if slices.Contains(s.Evidence.Initial, name) {
			continue
		}
		// A replay can only hand over an item, so a template that can't be
		// read stops the build.
		item, err := f.cat.Evidence.Collect(ctx, f.c, name, f.w, nil, s.Now)
		if err != nil {
			return Bundle{}, fmt.Errorf("%s: %w", name, err)
		}
		item.ID = "ev-" + name
		b.Requestable[name] = bundleItem(item)
	}
	for _, r := range f.d.Results {
		out := Check{Name: r.Name, Version: r.Version, Value: r.Value.String(), Reason: r.Reason, Blind: r.Blind}
		for _, id := range r.EvidenceIDs {
			out.EvidenceIDs = append(out.EvidenceIDs, ids[id])
		}
		for _, g := range r.Gaps {
			out.Gaps = append(out.Gaps, Gap{Template: g.Template, Cause: string(g.Cause)})
		}
		b.Predicates = append(b.Predicates, out)
	}
	var decider governance.Decider
	if b.Unproposed, err = decider.Unproposed(f.matched, f.cat.Fixes, f.d.Results, f.target.Namespace); err != nil {
		return Bundle{}, err
	}
	fix, err := f.caseFix(ctx, s)
	switch {
	case err != nil:
		return Bundle{}, err
	case fix == nil:
		b.Policy = PolicyCheck{Outcome: string(policy.Deny), Reason: "The workload has no earlier revision to roll back to."}
	case s.Fix == nil:
		b.Policy, err = f.govern(ctx, f.cat.Fixes[rollback.Name], fix.Params)
	default:
		b.Policy, err = f.decide(ctx, *s.Fix)
	}
	if err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// found is a case's cluster as Themis finds it during the incident: its
// target, the profiles the incident gets, and what their checks say.
type found struct {
	c       client.Reader
	target  incident.TargetRef
	w       *workload.Workload
	cat     catalog.Catalog
	matched config.Matched
	d       catalog.Diagnosis
}

// find reads the case in dir and diagnoses it as the core diagnoses an
// incident, under the profiles it gets, with the case's evidence.
func find(ctx context.Context, dir string, s Case, packs fs.FS, profile config.Profile) (found, error) {
	c, target, err := loadCluster(filepath.Join(dir, "cluster.yaml"), s.Target)
	if err != nil {
		return found{}, err
	}
	usage, err := loadMetrics(filepath.Join(dir, "metrics.yaml"))
	if err != nil {
		return found{}, err
	}
	cat, err := catalog.Build(fileLogs{dir: filepath.Join(dir, "logs")}, usage, packs, nil)
	if err != nil {
		return found{}, err
	}
	matched := config.Matched{profile}
	if s.Alert != "" {
		matched = config.MatchProfiles(cat.Profiles, s.Alert, nil, target)
		if !slices.ContainsFunc(matched, func(p config.Profile) bool { return p.Name == profile.Name }) {
			return found{}, fmt.Errorf("case.yaml: the alert %s doesn't match the pack's profile", s.Alert)
		}
	}
	w, err := workload.Read(ctx, c, target)
	if err != nil {
		return found{}, err
	}
	// No events have expired.
	scope := slices.Clone(matched)
	for i := range scope {
		scope[i].Evidence = config.Evidence{Initial: s.Evidence.Initial, Allowed: s.Evidence.Allowed}
	}
	d := cat.Diagnose(ctx, c, w, scope, catalog.Moment{Now: s.Now})
	return found{c: c, target: target, w: w, cat: cat, matched: matched, d: d}, nil
}

// caseFix is the case's own fix, or a rollback to the revision before the
// current one; nil when there is no earlier revision.
func (f found) caseFix(ctx context.Context, s Case) (*Fix, error) {
	if s.Fix != nil {
		return s.Fix, nil
	}
	revisions, err := f.w.Revisions(ctx)
	if err != nil {
		return nil, err
	}
	var previous int64
	for _, r := range revisions {
		if r.Number < f.w.Revision && r.Number > previous {
			previous = r.Number
		}
	}
	if previous == 0 {
		return nil, nil
	}
	return &Fix{Capability: rollback.Name, Params: map[string]any{"revision": previous}}, nil
}

// decide decides the case's own fix, which must be one the incident's
// profiles offer.
func (f found) decide(ctx context.Context, fix Fix) (PolicyCheck, error) {
	c, ok := f.cat.Fixes[fix.Capability]
	if !ok || len(f.matched.Offering(fix.Capability)) == 0 {
		return PolicyCheck{}, fmt.Errorf("fix: %s isn't a fix the pack's profile offers", fix.Capability)
	}
	out, err := f.govern(ctx, c, fix.Params)
	out.Fix = &fix
	return out, err
}

// govern decides fix with params as governance decides the investigator's
// suggestion. A fix that can't go ahead reaches nobody, as a denied one
// doesn't; params it can't take are the case's mistake.
func (f found) govern(ctx context.Context, fix capability.Capability, params map[string]any) (PolicyCheck, error) {
	var decider governance.Decider
	decided, err := decider.Decide(ctx, f.c, governance.Fix{
		Target: f.w.Target, Profiles: f.matched, Checks: f.d.Results,
		Capability: fix, Params: params,
		By: governance.Suggester{Source: "themis-intelligence", Authenticated: true},
	})
	if refusal, ok := errors.AsType[*capability.Refusal](err); ok {
		return PolicyCheck{Outcome: string(policy.Deny), Reason: refusal.Reason}, nil
	}
	if err != nil {
		return PolicyCheck{}, fmt.Errorf("fix: %w", err)
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
	out := []BundleFix{}
	for _, name := range allowed {
		f, ok := fixes[name]
		if !ok {
			continue
		}
		out = append(out, BundleFix{Name: f.Name(), Description: f.Description(), ParamsSchema: string(f.Schema())})
	}
	return out
}
