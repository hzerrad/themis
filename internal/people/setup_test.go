package people_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/people"
)

type setupStore struct {
	finished bool
	err      error
	asked    int
}

func (s *setupStore) SetupFinished(context.Context) (bool, error) {
	s.asked++
	return s.finished, s.err
}

func (s *setupStore) FinishSetup(context.Context, string, time.Time) error {
	s.finished = true
	return nil
}

// throughDoor sends req through a door and returns who Themis took the
// sender for, and the door's own status if it answered.
func throughDoor(t *testing.T, door *people.Door, req *http.Request) (people.Identity, int, error) {
	t.Helper()
	d := directory(t)
	var who people.Identity
	var err error
	door.Next = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { who, err = d.Identify(r) })
	door.Logger = slog.New(slog.DiscardHandler)
	rec := httptest.NewRecorder()
	door.ServeHTTP(rec, req)
	return who, rec.Code, err
}

func TestDoor_LetsInAsAdminUntilSetupFinishes(t *testing.T) {
	store := &setupStore{}
	door := &people.Door{Store: store}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:8090/v1/me", nil)
	who, _, err := throughDoor(t, door, req)
	if err != nil || who.Username != people.SetupUser || who.Role != config.RoleAdmin || !who.Setup {
		t.Fatalf("before setup: identity = %+v, %v; want the setup admin", who, err)
	}

	store.finished = true
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:8090/v1/me", nil)
	if who, _, err := throughDoor(t, door, req); !errors.Is(err, people.ErrSignedOut) {
		t.Fatalf("after setup: identity = %+v, %v; want signed out", who, err)
	}
	asked := store.asked
	_, _, _ = throughDoor(t, door, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:8090/", nil))
	if store.asked != asked {
		t.Error("the door asks the store again after setup finished; setup never reopens")
	}
}

func TestDoor_Refuses(t *testing.T) {
	for name, tc := range map[string]struct {
		method, url string
		storeErr    error
		status      int
		err         error
	}{
		// A page on another site that points its name at 127.0.0.1.
		"another host name": {method: http.MethodGet, url: "http://evil.example:8090/v1/incidents", status: http.StatusMisdirectedRequest},
		// A form on another site can post to localhost, but can't add the header.
		"a change without the header": {method: http.MethodPost, url: "http://127.0.0.1:8090/v1/actions/x/approve", err: people.ErrNoRequestHeader},
		"the store is down":           {method: http.MethodGet, url: "http://localhost:8090/v1/me", storeErr: errors.New("down"), status: http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.url, nil)
			who, status, err := throughDoor(t, &people.Door{Store: &setupStore{err: tc.storeErr}}, req)
			if tc.status != 0 {
				if status != tc.status {
					t.Errorf("status = %d, want %d", status, tc.status)
				}
				return
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("identity = %+v, %v; want %v", who, err, tc.err)
			}
		})
	}
}

func TestDoor_AcceptsLocalNames(t *testing.T) {
	for _, url := range []string{"http://localhost:8090/", "http://127.0.0.1:8090/", "http://[::1]:8090/", "http://localhost/"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, url, nil)
		req.Header.Set(people.RequestHeader, "1")
		if who, _, err := throughDoor(t, &people.Door{Store: &setupStore{}}, req); err != nil || !who.Setup {
			t.Errorf("%s: identity = %+v, %v; want the setup admin", url, who, err)
		}
	}
}

// Without the door, nothing is an admin for free.
func TestIdentify_NoSetupWithoutTheDoor(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:8090/v1/me", nil)
	if who, err := directory(t).Identify(req); !errors.Is(err, people.ErrSignedOut) {
		t.Errorf("identity = %+v, %v; want signed out", who, err)
	}
}

// A real session wins over the door, so a person who signs in is themselves.
func TestDoor_SessionWins(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:8090/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: people.SessionCookie, Value: "live", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if who, _, err := throughDoor(t, &people.Door{Store: &setupStore{}}, req); err != nil || who.Username != "bob@example.com" || who.Setup {
		t.Errorf("identity = %+v, %v; want bob", who, err)
	}
}
