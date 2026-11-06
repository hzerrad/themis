package statuspage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/store"
)

// State is how a part of the install is doing. People read OK as
// Operational, Warn as Action required and Problem as Failing.
type State string

const (
	OK      State = "ok"
	Warn    State = "warn"
	Problem State = "problem"
	// Waiting means nothing has happened yet that would tell.
	Waiting State = "waiting"
	// Off means the part isn't installed or turned on.
	Off State = "off"
)

// Check is one part of the install: a short finding, and the remedy once.
type Check struct {
	Name    string `json:"name"`
	State   State  `json:"state"`
	Summary string `json:"summary"`
	// Fix is the remedy, as an instruction; empty when there is nothing to do.
	Fix string `json:"fix,omitempty"`
	// At is when the part was last heard from, if that's what it rests on,
	// and Since names that moment, such as "Last request".
	At    *time.Time `json:"at,omitempty"`
	Since string     `json:"since,omitempty"`
}

// Store is what the checks read from the database.
type Store interface {
	Ping(ctx context.Context) error
	LastHeartbeat(ctx context.Context, component string) (store.Heartbeat, error)
	LastInvestigationOutcome(ctx context.Context) (store.InvestigationOutcome, error)
	HasAccounts(ctx context.Context) (bool, error)
	SetupFinished(ctx context.Context) (bool, error)
}

// Access says whether Themis may do what attrs describe.
type Access interface {
	Can(ctx context.Context, attrs authorizationv1.ResourceAttributes) (bool, error)
}

// Checker checks every part of an install.
type Checker struct {
	Config config.Provider
	Store  Store
	Access Access
	// Executor is the executor's address; empty when fixes are off.
	Executor string
	// OIDC says whether people can sign in through an identity provider.
	OIDC bool
	// NoInvestigator says no investigator is installed, by choice.
	NoInvestigator bool
	// Model says what model the install and setup set. Nil means only the
	// install sets it.
	Model interface {
		View(ctx context.Context) (settings.ModelView, error)
	}
	// Metrics are the sources Themis reads containers' memory and CPU use
	// from. Nil means none is set up.
	Metrics interface {
		Health(ctx context.Context) []metrics.Health
	}
	// Dial reaches the executor; nil dials TCP.
	Dial func(ctx context.Context, addr string) error
	Now  func() time.Time
}

// investigatorQuiet is how long the investigator may go without asking for
// work. It asks every few seconds, and its heartbeat is written every half
// minute.
const investigatorQuiet = 2 * time.Minute

const checkTimeout = 3 * time.Second

// Checks checks every part, in the order an operator sets them up.
func (c *Checker) Checks(ctx context.Context) []Check {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Store.Ping(ctx); err != nil {
		return []Check{{
			Name: "Database", State: Problem, Summary: "Unreachable: " + err.Error() + ". The other parts are not checked until it answers.",
			Fix: "Check that the database is running and that DATABASE_URL points at it.",
		}}
	}
	return []Check{
		{Name: "Database", State: OK, Summary: "Reachable."},
		c.investigator(ctx),
		c.model(ctx),
		c.alerts(ctx),
		c.namespaces(ctx),
		c.metrics(ctx),
		c.executor(ctx),
		c.signIn(ctx),
	}
}

func (c *Checker) investigator(ctx context.Context) Check {
	ch := Check{Name: "Investigator"}
	if c.NoInvestigator {
		ch.State, ch.Summary = Off, "Not installed. Incidents wait for a fix suggested by a person or an outside tool, such as HolmesGPT."
		ch.Fix = installInvestigator
		return ch
	}
	h, err := c.Store.LastHeartbeat(ctx, store.HeartbeatInvestigator)
	switch {
	case errors.Is(err, store.ErrNotFound):
		ch.State, ch.Summary = Problem, "Not connected. No request for work yet."
		ch.Fix = "Check that the themis-intelligence pod is running, and read its logs."
	case err != nil:
		return failed(ch, err)
	case c.now().Sub(h.SeenAt) > investigatorQuiet:
		ch.State, ch.Summary, ch.At, ch.Since = Problem, "Disconnected.", &h.SeenAt, "Last request"
		ch.Fix = "Check that the themis-intelligence pod is running, and read its logs."
	default:
		ch.State, ch.Summary, ch.At, ch.Since = OK, "Connected.", &h.SeenAt, "Last request"
	}
	return ch
}

// installInvestigator is the remedy when no investigator is installed.
const installInvestigator = "Set model.enabled=true and the model's address to have Themis investigate."

// modelFixes say what to do about each way the model can fail.
var modelFixes = map[string]struct{ summary, fix string }{
	"model_unavailable": {
		"Unreachable in the last investigation.",
		"Check model.baseURL and the API key, and that model.egressCIDR includes the model's address.",
	},
	"invalid_model_output": {
		"Answers in the last investigation didn't follow Themis's reply format.",
		"For OpenAI models, set model.schemaDialect=openai-strict. For others, use a larger model.",
	},
	"deadline_exceeded": {
		"Too slow. The last investigation ran out of time.",
		"Use a faster model, or one closer to the cluster.",
	},
	"stopped_responding": {
		"The investigator stopped responding in the last investigation.",
		"Read the themis-intelligence pod's logs, and check whether it restarts, such as for running out of memory.",
	},
}

func (c *Checker) model(ctx context.Context) Check {
	ch := Check{Name: "Model"}
	if c.NoInvestigator {
		ch.State, ch.Summary = Off, "Not used. No investigator is installed."
		ch.Fix = installInvestigator
		return ch
	}
	if c.Model != nil {
		v, err := c.Model.View(ctx)
		if err != nil {
			return failed(ch, err)
		}
		if !v.Complete {
			ch.State, ch.Summary = Problem, "Not set. Incidents are not investigated."
			ch.Fix = "Set the model's address and name in Setup, or in the chart's model values."
			return ch
		}
	}
	o, err := c.Store.LastInvestigationOutcome(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		ch.State, ch.Summary = Waiting, "Not used yet."
		return ch
	case err != nil:
		return failed(ch, err)
	}
	ch.At, ch.Since = &o.At, "Last investigation"
	if o.Completed {
		ch.State, ch.Summary, ch.Since = OK, "Answering.", "Last answer"
		if o.Model != "" {
			// The model's name as written: which model answers is the finding.
			ch.Summary = o.Model + "."
		}
		return ch
	}
	if o.FailureKind == "budget_exhausted" {
		ch.State, ch.Summary = Warn, "The last investigation used its token budget before deciding."
		ch.Fix = "Raise the incident profile's token budget if this recurs."
		return ch
	}
	if f, ok := modelFixes[o.FailureKind]; ok {
		ch.State, ch.Summary, ch.Fix = Problem, f.summary, f.fix
		return ch
	}
	ch.State, ch.Summary = Problem, strings.TrimSuffix(o.Failure, ".")+"."
	ch.Fix = "Read the themis-intelligence pod's logs."
	return ch
}

func (c *Checker) alerts(ctx context.Context) Check {
	ch := Check{Name: "Alerts"}
	got, err := c.Store.LastHeartbeat(ctx, store.HeartbeatAlertmanager)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return failed(ch, err)
	}
	refused, rerr := c.Store.LastHeartbeat(ctx, store.HeartbeatAlertmanagerRefused)
	if rerr != nil && !errors.Is(rerr, store.ErrNotFound) {
		return failed(ch, rerr)
	}
	if rerr == nil && (err != nil || refused.SeenAt.After(got.SeenAt)) {
		ch.State, ch.Summary, ch.At, ch.Since = Problem, "Wrong intake token.", &refused.SeenAt, "Last refused"
		ch.Fix = "Set the receiver's token to the intake key of the themis-tokens Secret."
		return ch
	}
	if err != nil {
		ch.State, ch.Summary = Waiting, "No alert received."
		ch.Fix = "Add an Alertmanager webhook receiver for Themis's /v1/intake/alertmanager, with the intake token from the themis-tokens Secret. Route crash-loop and out-of-memory alerts to it."
		return ch
	}
	ch.State, ch.Summary, ch.At, ch.Since = OK, "Received.", &got.SeenAt, "Last alert"
	if got.Detail != "" {
		ch.Summary = fmt.Sprintf("Received through the %q receiver.", got.Detail)
	}
	return ch
}

// needs is what Themis reads in each namespace it watches, and what's lost
// without it.
var needs = []struct {
	attrs authorizationv1.ResourceAttributes
	lost  string
	// optional means Themis works without it, only seeing less.
	optional bool
}{
	{authorizationv1.ResourceAttributes{Verb: "list", Resource: "pods"}, "list pods", false},
	{authorizationv1.ResourceAttributes{Verb: "get", Group: "apps", Resource: "deployments"}, "read deployments", false},
	{authorizationv1.ResourceAttributes{Verb: "list", Group: "apps", Resource: "replicasets"}, "list replica sets", false},
	{authorizationv1.ResourceAttributes{Verb: "list", Resource: "events"}, "list events", true},
	{authorizationv1.ResourceAttributes{Verb: "get", Resource: "pods", Subresource: "log"}, "read container logs", true},
}

func (c *Checker) namespaces(ctx context.Context) Check {
	ch := Check{Name: "Namespaces"}
	enrolled := c.Config.Current().EnrolledNamespaces
	if len(enrolled) == 0 {
		ch.State, ch.Summary, ch.Fix = Problem, "None watched.", "List the namespaces to watch in enrolledNamespaces."
		return ch
	}
	var problems, warnings []string
	for _, ns := range enrolled {
		var missing, optional []string
		for _, n := range needs {
			attrs := n.attrs
			attrs.Namespace = ns
			cctx, cancel := context.WithTimeout(ctx, checkTimeout)
			ok, err := c.Access.Can(cctx, attrs)
			cancel()
			if err != nil {
				return failed(ch, fmt.Errorf("ask Kubernetes what Themis may do in %s: %w", ns, err))
			}
			switch {
			case ok:
			case n.optional:
				optional = append(optional, n.lost)
			default:
				missing = append(missing, n.lost)
			}
		}
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("Not allowed in %s: %s.", ns, join(missing, "or")))
		}
		if len(optional) > 0 {
			warnings = append(warnings, fmt.Sprintf("Not allowed in %s: %s.", ns, join(optional, "or")))
		}
	}
	switch {
	case len(problems) > 0:
		ch.State, ch.Summary = Problem, strings.Join(append(problems, warnings...), " ")
		ch.Fix = "Check that each namespace exists, then upgrade the chart to grant access there. Where access is managed outside the chart, grant the core's service account get and list on pods, deployments and replica sets."
	case len(warnings) > 0:
		ch.State, ch.Summary = Warn, strings.Join(warnings, " ")+" Investigations there see less evidence."
		ch.Fix = "Grant the core's service account get on pods/log and list on events there, unless this is intended."
	default:
		ch.State, ch.Summary = OK, "Readable: "+strings.Join(enrolled, ", ")+"."
	}
	return ch
}

// metrics says which sources say how much memory and CPU containers use,
// and what Themis can't see without one that keeps history.
func (c *Checker) metrics(ctx context.Context) Check {
	ch := Check{Name: "Metrics"}
	var health []metrics.Health
	if c.Metrics != nil {
		mctx, cancel := context.WithTimeout(ctx, checkTimeout)
		health = c.Metrics.Health(mctx)
		cancel()
	}
	if len(health) == 0 {
		ch.State, ch.Summary = Warn, "No metrics source."
		ch.Fix = "Set metrics.prometheus.address, or turn the Kubernetes metrics API back on with metrics.kubernetes."
		return ch
	}
	var using, broken []string
	history, promBroken := false, false
	for _, h := range health {
		switch {
		case errors.Is(h.Err, metrics.ErrNotServed):
			// Not a source in this cluster, rather than one that fails.
		case h.Err != nil:
			broken = append(broken, upper(strings.TrimSuffix(h.Err.Error(), "."))+".")
			promBroken = promBroken || h.History
		default:
			using = append(using, c.sourceText(h.Source))
			history = history || h.History
		}
	}
	var says []string
	if len(using) > 0 {
		says = append(says, fmt.Sprintf("Read from %s.", join(using, "and")))
	}
	says = append(says, broken...)
	switch {
	case promBroken:
		ch.State = Problem
		ch.Fix = "Check metrics.prometheus.address, its token and TLS settings, and that Themis's pod can reach it."
	case len(using) == 0 && len(broken) == 0:
		ch.State, says = Warn, []string{"No metrics source."}
		ch.Fix = "Install metrics-server, or set metrics.prometheus.address."
	case len(using) == 0:
		ch.State = Warn
		ch.Fix = "Install metrics-server, or set metrics.prometheus.address."
	case !history:
		ch.State = OK
		says = append(says, "No history. Peak memory and CPU throttling are unknown.")
		ch.Fix = "Set metrics.prometheus.address to read them."
	default:
		ch.State = OK
	}
	ch.Summary = strings.Join(says, " ")
	return ch
}

// sourceText names a metrics source for people.
func (c *Checker) sourceText(name string) string {
	switch name {
	case metrics.Kubernetes:
		return "the Kubernetes metrics API"
	case metrics.Prometheus:
		if p := c.Config.Current().Metrics.Prometheus; p != nil {
			if u, err := url.Parse(p.Address); err == nil {
				return "Prometheus at " + u.Redacted()
			}
		}
		return "Prometheus"
	}
	return name
}

func (c *Checker) executor(ctx context.Context) Check {
	ch := Check{Name: "Fixes"}
	if c.Executor == "" {
		ch.State, ch.Summary = Off, "Off. Fixes are proposed, never applied."
		ch.Fix = "Set executor.enabled=true to apply fixes a person approves."
		return ch
	}
	dctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	dial := c.Dial
	if dial == nil {
		dial = dialTCP
	}
	if err := dial(dctx, c.Executor); err != nil {
		ch.State, ch.Summary = Problem, fmt.Sprintf("Executor unreachable at %s. Approved fixes can't be applied.", c.Executor)
		ch.Fix = "Check that the themis-executor pod is running and ready."
		return ch
	}
	ch.State, ch.Summary = OK, "Enabled. Applied only after a person approves."
	return ch
}

func dialTCP(ctx context.Context, addr string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func (c *Checker) signIn(ctx context.Context) Check {
	ch := Check{Name: "Sign-in"}
	finished, err := c.Store.SetupFinished(ctx)
	if err != nil {
		return failed(ch, err)
	}
	accounts, err := c.Store.HasAccounts(ctx)
	if err != nil {
		return failed(ch, err)
	}
	var ways []string
	if c.OIDC {
		ways = append(ways, "the identity provider")
	}
	if accounts {
		ways = append(ways, "Themis accounts")
	}
	switch {
	case !finished:
		ch.State, ch.Summary = Warn, "Port-forward grants admin."
		ch.Fix = "Create an admin account and sign in with it, or configure the identity provider."
	case len(ways) == 0:
		ch.State, ch.Summary = Warn, "Only the break-glass token signs in."
		ch.Fix = "Create accounts, or configure the identity provider."
	default:
		ch.State, ch.Summary = OK, "Through "+strings.Join(ways, " and ")+"."
	}
	return ch
}

func failed(ch Check, err error) Check {
	ch.State, ch.Summary = Problem, "Not checked: "+err.Error()+"."
	ch.Fix = "Check again. If this recurs, read the core's logs."
	return ch
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// join joins items as English: "a, b and c", or with "or".
func join(items []string, conjunction string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + conjunction + " " + items[len(items)-1]
}

// KubeAccess asks the Kubernetes API server what Themis's own service account
// may do. Anyone may ask that about themselves, so it needs no permission.
type KubeAccess struct {
	Clientset kubernetes.Interface
}

func (k KubeAccess) Can(ctx context.Context, attrs authorizationv1.ResourceAttributes) (bool, error) {
	review, err := k.Clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx,
		&authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return review.Status.Allowed, nil
}
