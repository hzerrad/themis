// Package evaluation builds what the evaluation replays: each scenario's
// cluster, read by the real evidence templates and checks, written as a bundle
// a model can be run against with no cluster.
package evaluation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/grpcserver"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
)

// Decisions a scenario can expect of an investigation.
const (
	DecisionPropose = "propose"
	DecisionAbstain = "abstain"
	DecisionEither  = "either"
)

// Scenario is what scenario.yaml declares. Changing Truth means a new Version,
// so results are never compared across different answers.
type Scenario struct {
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

	// Predicates are the checks' right answers. Those not listed aren't
	// compared.
	Predicates map[string]string `json:"predicates"`
}

// Validate reports every problem in s, not only the first.
func (s Scenario) Validate() error {
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

// Load reads and validates the scenario in dir. Unknown keys are errors.
func Load(dir string) (Scenario, error) {
	data, err := os.ReadFile(filepath.Join(dir, "scenario.yaml")) //nolint:gosec // a path in the repository
	if err != nil {
		return Scenario{}, err
	}
	var s Scenario
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return Scenario{}, fmt.Errorf("%s: %w", dir, err)
	}
	if err := s.Validate(); err != nil {
		return Scenario{}, fmt.Errorf("%s: %w", dir, err)
	}
	if s.Name != filepath.Base(dir) {
		return Scenario{}, fmt.Errorf("%s: name %q must match the directory", dir, s.Name)
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

	Capabilities        []string `json:"capabilities"`
	MaxSteps            int      `json:"max_steps"`
	MaxEvidenceRequests int      `json:"max_evidence_requests"`

	// Initial is what the investigation starts with; Requestable is what it
	// may ask for, by template.
	Initial     []Item          `json:"initial"`
	Requestable map[string]Item `json:"requestable"`
	Templates   []Template      `json:"templates"`

	Predicates []Check `json:"predicates"`
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

// Check is one check's answer.
type Check struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Value       string   `json:"value"`
	Reason      string   `json:"reason"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

// Build reads the scenario in dir with the real templates and checks.
func Build(ctx context.Context, dir string, s Scenario) (Bundle, error) {
	c, target, err := loadCluster(filepath.Join(dir, "cluster.yaml"), s.Target)
	if err != nil {
		return Bundle{}, err
	}
	registry := evidence.NewRegistry(
		evidence.NewRolloutHistory(), evidence.NewPodStates(), evidence.NewTargetEvents(),
		evidence.NewReferencedConfig(), evidence.NewContainerLogsTail(fileLogs{dir: filepath.Join(dir, "logs")}),
	)

	b := Bundle{
		Scenario: s.Name, Version: s.Version, Difficulty: s.Difficulty, IncidentClass: s.IncidentClass,
		Truth: s.Truth, Target: target, Capabilities: config.KnownCapabilities,
		MaxSteps: config.DefaultMaxSteps, MaxEvidenceRequests: config.DefaultMaxEvidenceRequests,
		Requestable: map[string]Item{},
	}
	var initial evidence.Bundle
	initial.Target = target
	for _, name := range s.Evidence.Allowed {
		item, err := registry.Collect(ctx, c, name, target, nil, s.Now)
		if err != nil {
			return Bundle{}, fmt.Errorf("%s: %w", name, err)
		}
		item.ID = "ev-" + name
		about := grpcserver.Templates[name]
		b.Templates = append(b.Templates, Template{Name: name, Description: about.GetDescription(), Params: about.GetParams()})
		out := Item{
			ID: item.ID, Template: name, Data: string(item.Data), Truncated: item.Truncated,
			OriginalBytes: item.OriginalBytes, Redactions: item.Redactions, Classification: string(item.Classification),
		}
		if slices.Contains(s.Evidence.Initial, name) {
			b.Initial = append(b.Initial, out)
			initial.Items = append(initial.Items, item)
		} else {
			b.Requestable[name] = out
		}
	}
	for _, r := range predicate.Evaluate(initial, s.Now) {
		b.Predicates = append(b.Predicates, Check{Name: r.Name, Version: r.Version, Value: r.Value.String(), Reason: r.Reason, EvidenceIDs: r.EvidenceIDs})
	}
	return b, nil
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
	builder := fake.NewClientBuilder().WithScheme(scheme)
	target := incident.TargetRef{}
	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, incident.TargetRef{}, fmt.Errorf("%s: %w", path, err)
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
			return nil, incident.TargetRef{}, fmt.Errorf("%s: %w", path, err)
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
		return nil, incident.TargetRef{}, fmt.Errorf("%s has no %s %s", path, want.Kind, want.Name)
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
