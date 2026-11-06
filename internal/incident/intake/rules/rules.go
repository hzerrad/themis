// Package rules is about the Prometheus alerting rules behind the alerts the
// profiles answer: whether a Prometheus has them, and the rules Themis ships
// for them, for the alerts the usual stacks lack.
package rules

import (
	"context"
	"crypto/tls"
	_ "embed"
	"fmt"
	"net/url"
	"slices"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/platform/fetch"
)

// File is the rules Themis ships, as a Prometheus rule file.
//
//go:embed rules.yaml
var File []byte

// Groups of the shipped rules.
const (
	// Group holds the rules the usual stacks lack.
	Group = "themis"
	// StandardGroup holds those kube-prometheus-stack has.
	StandardGroup = "themis-standard"
)

// RuleFile is a Prometheus rule file.
type RuleFile struct {
	Groups []RuleGroup `json:"groups"`
}

// RuleGroup is a group of rules in a rule file.
type RuleGroup struct {
	Name  string `json:"name"`
	Rules []Rule `json:"rules"`
}

// Rule is an alerting rule.
type Rule struct {
	Alert       string            `json:"alert"`
	Expr        string            `json:"expr"`
	For         string            `json:"for,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Shipped returns the rules Themis ships.
func Shipped() (RuleFile, error) {
	var f RuleFile
	if err := yaml.UnmarshalStrict(File, &f); err != nil {
		return RuleFile{}, fmt.Errorf("read the shipped alerting rules: %w", err)
	}
	return f, nil
}

// For returns the shipped rules for the alerts named, in one group, and the
// names Themis ships no rule for. It says whether any is one
// kube-prometheus-stack has.
func (f RuleFile) For(names []string) (out RuleFile, standard bool, none []string) {
	group := RuleGroup{Name: Group}
	for _, name := range names {
		found := false
		for _, g := range f.Groups {
			for _, r := range g.Rules {
				if r.Alert == name && !found {
					group.Rules = append(group.Rules, r)
					found, standard = true, standard || g.Name == StandardGroup
				}
			}
		}
		if !found {
			none = append(none, name)
		}
	}
	if len(group.Rules) > 0 {
		out.Groups = []RuleGroup{group}
	}
	return out, standard, none
}

// YAML writes the rules as a Prometheus rule file.
func (f RuleFile) YAML() string {
	b, _ := yaml.Marshal(f)
	return string(b)
}

// PrometheusRule writes the rules as a Prometheus Operator PrometheusRule
// called name.
func (f RuleFile) PrometheusRule(name string) string {
	b, _ := yaml.Marshal(map[string]any{
		"apiVersion": "monitoring.coreos.com/v1",
		"kind":       "PrometheusRule",
		"metadata":   map[string]any{"name": name},
		"spec":       f,
	})
	return string(b)
}

// Prometheus reads the alerting rules of a server that answers Prometheus's
// API: Prometheus, Thanos, Mimir or VictoriaMetrics.
type Prometheus struct {
	client *fetch.Client
}

const (
	// requestTimeout bounds reading the rules, which the status page waits
	// for.
	requestTimeout = 3 * time.Second
	// maxRulesBytes bounds the rules: thousands, with their expressions.
	maxRulesBytes = 16 << 20
)

// NewPrometheus reads the rules at address, with creds, checking TLS as
// tlsConfig says.
func NewPrometheus(address string, creds fetch.Credentials, tlsConfig *tls.Config) (*Prometheus, error) {
	c, err := fetch.New("Prometheus", address, creds, tlsConfig, requestTimeout)
	if err != nil {
		return nil, err
	}
	return &Prometheus{client: c}, nil
}

// Result says which of the alerts answered have a rule in Prometheus.
type Result struct {
	Address string   `json:"address"`
	Exist   []string `json:"exist"`
	Missing []string `json:"missing"`
}

// Check reads which of the alerts answered have an alerting rule.
func (p *Prometheus) Check(ctx context.Context, answered []string) (Result, error) {
	q := url.Values{"type": {"alert"}}
	for _, name := range answered {
		q.Add("rule_name[]", name)
	}
	var out struct {
		Data struct {
			Groups []struct {
				Rules []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := p.client.Get(ctx, "api/v1/rules", q, maxRulesBytes, &out); err != nil {
		return Result{}, err
	}
	have := map[string]bool{}
	for _, g := range out.Data.Groups {
		for _, r := range g.Rules {
			have[r.Name] = have[r.Name] || r.Type == "alerting"
		}
	}
	res := Result{Address: p.client.Address(), Exist: []string{}, Missing: []string{}}
	for _, name := range answered {
		if have[name] {
			res.Exist = append(res.Exist, name)
		} else {
			res.Missing = append(res.Missing, name)
		}
	}
	slices.Sort(res.Exist)
	slices.Sort(res.Missing)
	return res, nil
}
