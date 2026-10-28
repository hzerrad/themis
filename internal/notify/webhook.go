// Package notify tells humans when Themis hands an incident to them.
package notify

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/auth"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/store"
)

// timeout bounds one delivery; River retries a failed one.
const timeout = 10 * time.Second

// Worker delivers escalation notices to the configured webhook.
type Worker struct {
	river.WorkerDefaults[store.EscalationArgs]

	Store  *store.Store
	Config config.Notifications
	Client *http.Client
}

func (w *Worker) Work(ctx context.Context, job *river.Job[store.EscalationArgs]) error {
	return w.Notify(ctx, job.Args)
}

type payload struct {
	Event    string       `json:"event"`
	Incident incidentView `json:"incident"`
}

type incidentView struct {
	ID      string     `json:"id"`
	State   string     `json:"state"`
	From    string     `json:"from"`
	Reason  string     `json:"reason"`
	Profile string     `json:"profile"`
	Target  targetView `json:"target"`
	At      time.Time  `json:"at"`
	URL     string     `json:"url,omitempty"`
}

type targetView struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// Notify posts the notice; anything but a 2xx answer is an error to retry.
func (w *Worker) Notify(ctx context.Context, n store.EscalationArgs) error {
	hook := w.Config.Webhook
	if hook == nil {
		return nil
	}
	in, err := w.Store.Incident(ctx, n.IncidentID)
	if err != nil {
		return err
	}
	t := in.Target
	p := payload{
		Event: "incident." + string(n.To),
		Incident: incidentView{
			ID: in.ID, State: string(n.To), From: string(n.From), Reason: n.Reason, Profile: in.Profile, At: n.At,
			Target: targetView{Group: t.Group, Kind: t.Kind, Namespace: t.Namespace, Name: t.Name, UID: t.UID},
		},
	}
	if base := w.Config.ExternalURL; base != "" {
		p.Incident.URL = strings.TrimRight(base, "/") + "/#/incidents/" + in.ID
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}

	target := hook.URL
	if hook.URLFile != "" {
		b, err := auth.ReadToken(hook.URLFile)
		if err != nil {
			return fmt.Errorf("webhook URL: %w", err)
		}
		target = string(b)
	}
	if !config.IsHTTPURL(target) {
		return errors.New("the webhook URL is not an http or https URL")
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if hook.BearerTokenFile != "" {
		token, err := auth.ReadToken(hook.BearerTokenFile)
		if err != nil {
			return fmt.Errorf("webhook token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
	}
	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL may hold a key: report the cause, never the URL.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("post escalation of %s: %w", in.ID, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("post escalation of %s: the receiver answered %s", in.ID, resp.Status)
	}
	return nil
}
