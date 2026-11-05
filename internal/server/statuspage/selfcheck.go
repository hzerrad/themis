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

// State is how a part of the install is doing.
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

// Check is one part of the install, said plainly.
type Check struct {
	Name    string `json:"name"`
	State   State  `json:"state"`
	Summary string `json:"summary"`
	// Fix says what to do when the state isn't OK.
	Fix string `json:"fix,omitempty"`
	// At is when the part was last heard from, if that's what it rests on.
	At *time.Time `json:"at,omitempty"`
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
			Name: "Database", State: Problem, Summary: "Themis can't reach its database: " + err.Error() + ".",
			Fix: "Check the database is running and that DATABASE_URL points at it. Nothing else can be checked until it answers.",
		}}
	}
	return []Check{
		{Name: "Database", State: OK, Summary: "Themis's database answers."},
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
		ch.State, ch.Summary = Off, "None is installed: each incident waits for a fix suggested by a person or a tool outside Themis, such as HolmesGPT."
		ch.Fix = "To have Themis diagnose incidents itself, install the investigator with a model: model.enabled=true and the model's address."
		return ch
	}
	h, err := c.Store.LastHeartbeat(ctx, store.HeartbeatInvestigator)
	switch {
	case errors.Is(err, store.ErrNotFound):
		ch.State, ch.Summary = Problem, "The investigator hasn't asked Themis for work yet."
		ch.Fix = "Check that the themis-intelligence pod is running, and read its logs: it should connect to the core within seconds of starting."
	case err != nil:
		return failed(ch, err)
	case c.now().Sub(h.SeenAt) > investigatorQuiet:
		ch.State, ch.Summary, ch.At = Problem, "The investigator has stopped asking Themis for work.", &h.SeenAt
		ch.Fix = "Check that the themis-intelligence pod is running, and read its logs."
	default:
		ch.State, ch.Summary, ch.At = OK, "The investigator is asking Themis for work.", &h.SeenAt
	}
	return ch
}

// modelFixes say what to do about each way the model can fail.
var modelFixes = map[string]struct{ summary, fix string }{
	"model_unavailable": {
		"The model couldn't be reached during the last investigation.",
		"Check the model's address (model.baseURL), its key, and that the investigator may reach it: the chart's model.egressCIDR must include the model's address.",
	},
	"invalid_model_output": {
		"The model's answers weren't in the format Themis asks for during the last investigation.",
		"OpenAI's models need model.strictOutput=false. Otherwise the model may be too small to follow Themis's reply format; try a larger one.",
	},
	"deadline_exceeded": {
		"The model was too slow to finish the last investigation in time.",
		"Use a faster model, or one closer to the cluster.",
	},
	"stopped_responding": {
		"The investigator stopped responding during the last investigation.",
		"Read the themis-intelligence pod's logs, and check it isn't being restarted, for example for running out of memory.",
	},
}

func (c *Checker) model(ctx context.Context) Check {
	ch := Check{Name: "Model"}
	if c.NoInvestigator {
		ch.State, ch.Summary = Off, "None: without an investigator, Themis asks no model."
		ch.Fix = "The model comes with the investigator: model.enabled=true and the model's address."
		return ch
	}
	if c.Model != nil {
		v, err := c.Model.View(ctx)
		if err != nil {
			return failed(ch, err)
		}
		if !v.Complete {
			ch.State, ch.Summary = Problem, "No model is set, so incidents aren't investigated."
			ch.Fix = "Set the model's address and name on the Setup page, or in the chart's model values."
			return ch
		}
	}
	o, err := c.Store.LastInvestigationOutcome(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		ch.State, ch.Summary = Waiting, "Not used yet. Themis asks the model when an incident needs a diagnosis."
		return ch
	case err != nil:
		return failed(ch, err)
	}
	ch.At = &o.At
	if o.Completed {
		ch.State, ch.Summary = OK, "The model answered the last investigation."
		if o.Model != "" {
			ch.Summary = fmt.Sprintf("The model, %s, answered the last investigation.", o.Model)
		}
		return ch
	}
	if o.FailureKind == "budget_exhausted" {
		ch.State, ch.Summary = Warn, "The model answered, but the last investigation used up its token budget before it decided."
		ch.Fix = "If this happens often, raise the incident profile's token budget."
		return ch
	}
	if f, ok := modelFixes[o.FailureKind]; ok {
		ch.State, ch.Summary, ch.Fix = Problem, f.summary, f.fix
		return ch
	}
	ch.State, ch.Summary = Problem, strings.TrimSuffix(o.Failure, ".")+"."
	ch.Fix = "Read the themis-intelligence pod's logs for the details."
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
		ch.State, ch.Summary, ch.At = Problem, "Something is sending alerts with the wrong token, and Themis refuses them.", &refused.SeenAt
		ch.Fix = "Give Alertmanager's Themis receiver the intake token from the themis-tokens Secret (key intake)."
		return ch
	}
	if err != nil {
		ch.State, ch.Summary = Waiting, "No alert has arrived from Alertmanager yet."
		ch.Fix = "Add a webhook receiver to Alertmanager for Themis's /v1/intake/alertmanager, with the intake token from the themis-tokens Secret, and route crash-loop and out-of-memory alerts to it."
		return ch
	}
	ch.State, ch.Summary, ch.At = OK, "Alertmanager is sending alerts.", &got.SeenAt
	if got.Detail != "" {
		ch.Summary = fmt.Sprintf("Alertmanager is sending alerts, through its %q receiver.", got.Detail)
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
		ch.State, ch.Summary, ch.Fix = Problem, "Themis watches no namespaces.", "List the namespaces to watch in enrolledNamespaces."
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
			problems = append(problems, fmt.Sprintf("in %s, Themis can't %s", ns, join(missing, "or")))
		}
		if len(optional) > 0 {
			warnings = append(warnings, fmt.Sprintf("in %s, Themis can't %s", ns, join(optional, "or")))
		}
	}
	switch {
	case len(problems) > 0:
		ch.State, ch.Summary = Problem, upper(strings.Join(append(problems, warnings...), "; "))+"."
		ch.Fix = "Make sure each namespace exists, then upgrade the chart so it grants Themis access there. If you manage access yourself, give the core's service account get and list on pods, deployments and replica sets."
	case len(warnings) > 0:
		ch.State, ch.Summary = Warn, upper(strings.Join(warnings, "; "))+". Investigations there will see less."
		ch.Fix = "If that isn't on purpose, grant the core's service account get on pods/log and list on events in those namespaces."
	default:
		ch.State, ch.Summary = OK, fmt.Sprintf("Themis can read %s.", join(enrolled, "and"))
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
		ch.State, ch.Summary = Off, "No metrics source is set up, so investigations can't see how much memory and CPU containers use."
		ch.Fix = "Set a Prometheus address (metrics.prometheus.address), or turn the Kubernetes metrics API back on (metrics.kubernetes)."
		return ch
	}
	var using, broken []string
	history, promBroken := false, false
	for _, h := range health {
		if h.Err != nil {
			broken = append(broken, upper(strings.TrimSuffix(h.Err.Error(), "."))+".")
			promBroken = promBroken || h.History
			continue
		}
		using = append(using, c.sourceText(h.Source))
		history = history || h.History
	}
	var says []string
	if len(using) > 0 {
		says = append(says, fmt.Sprintf("Memory and CPU use come from %s.", join(using, "and")))
		if !history {
			says = append(says, "No peak memory or CPU throttling data: the Kubernetes metrics API keeps no history. Set a Prometheus address (metrics.prometheus.address) to see them.")
		}
	}
	says = append(says, broken...)
	ch.Summary = strings.Join(says, " ")
	switch {
	case promBroken:
		ch.State = Problem
		ch.Fix = "Check metrics.prometheus.address, its token and TLS settings, and that Themis's pod may reach it."
	case len(using) == 0:
		ch.State = Warn
		ch.Fix = "Install metrics-server, or set a Prometheus address (metrics.prometheus.address)."
	default:
		ch.State = OK
	}
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
		ch.State, ch.Summary = Off, "Off: Themis diagnoses and suggests fixes, but never applies them."
		ch.Fix = "To let Themis apply fixes a person approves, install the executor with executor.enabled=true."
		return ch
	}
	dctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	dial := c.Dial
	if dial == nil {
		dial = dialTCP
	}
	if err := dial(dctx, c.Executor); err != nil {
		ch.State, ch.Summary = Problem, fmt.Sprintf("Themis can't reach the executor at %s, so approved fixes can't be applied.", c.Executor)
		ch.Fix = "Check that the themis-executor pod is running and ready."
		return ch
	}
	ch.State, ch.Summary = OK, "On: Themis applies fixes after a person approves them."
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
		ways = append(ways, "through your identity provider")
	}
	if accounts {
		ways = append(ways, "with Themis accounts")
	}
	switch {
	case !finished:
		ch.State, ch.Summary = Warn, "Setup isn't finished: anyone who can kubectl port-forward to Themis is an admin."
		ch.Fix = "Create an admin account on the Accounts page and sign in with it, or sign in as an admin through your identity provider."
	case len(ways) == 0:
		ch.State, ch.Summary = Warn, "Only the emergency token signs in."
		ch.Fix = "Create accounts on the Accounts page, or set up your identity provider, so people sign in as themselves."
	default:
		ch.State, ch.Summary = OK, "People sign in "+strings.Join(ways, " and ")+"."
	}
	return ch
}

func failed(ch Check, err error) Check {
	ch.State, ch.Summary = Problem, "Themis couldn't check this: "+err.Error()+"."
	ch.Fix = "Try again shortly. If it persists, the core's logs have the details."
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
