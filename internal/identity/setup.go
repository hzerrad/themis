package identity

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/hzerrad/themis/internal/telemetry"
)

// SetupUser is the username of whoever comes in through the setup door. No
// one may sign in as it.
const SetupUser = "setup"

// SetupStore says whether setup has finished, and records when it does.
type SetupStore interface {
	SetupFinished(ctx context.Context) (bool, error)
	FinishSetup(ctx context.Context, by string, at time.Time) error
}

// Door serves Themis on a port bound to the pod's loopback address, which only
// kubectl port-forward reaches. Kubernetes has already decided who may
// port-forward to Themis, so until an admin has signed in through a real
// sign-in, whoever comes in this way is an admin. After that, it serves Themis
// like any other address.
type Door struct {
	Store  SetupStore
	Next   http.Handler
	Logger *slog.Logger

	finished atomic.Bool
}

func (d *Door) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A page on another site that points its own name at 127.0.0.1 sends
	// that name, not localhost, so it can't use the door from a browser.
	if !loopbackHost(r.Host) {
		http.Error(w, "Open Themis at the localhost address kubectl port-forward printed.", http.StatusMisdirectedRequest)
		return
	}
	open, err := d.open(r.Context())
	if err != nil {
		d.logger().ErrorContext(r.Context(), "cannot check whether setup has finished", telemetry.FieldError, err)
		http.Error(w, "Themis can't check sign-ins right now. Try again shortly.", http.StatusServiceUnavailable)
		return
	}
	if open {
		r = r.WithContext(context.WithValue(r.Context(), setupKey{}, true))
	}
	d.Next.ServeHTTP(w, r)
}

func (d *Door) open(ctx context.Context) (bool, error) {
	if d.finished.Load() {
		return false, nil
	}
	finished, err := d.Store.SetupFinished(ctx)
	if err != nil {
		return false, err
	}
	// Setup never reopens, so once finished, the door stops asking.
	d.finished.Store(finished)
	return !finished, nil
}

func (d *Door) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

type setupKey struct{}

// throughSetup reports whether r came in through the door while setup is open.
func throughSetup(r *http.Request) bool {
	open, _ := r.Context().Value(setupKey{}).(bool)
	return open
}
