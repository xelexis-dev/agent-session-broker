package controlpanel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestSuspendControlsFollowStateAndPermission(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		for _, permitted := range []bool{false, true} {
			v := page{View: "sessions", Project: "demo", Sessions: []Session{{ID: "registration-1", Name: "target", Status: "alive", Suspended: suspended}}, CanManage: map[string]bool{"registration-1": permitted}}
			v.User.Username = "owner"
			if suspended {
				v.Sessions[0].Status = "suspended"
				v.Sessions[0].ErrorCode = "suspended"
			}
			w := httptest.NewRecorder()
			if err := pages.ExecuteTemplate(w, "page", v); err != nil {
				t.Fatal(err)
			}
			html := w.Body.String()
			wantSuspend := permitted && !suspended
			wantUnsuspend := permitted && suspended
			if strings.Contains(html, `action="/sessions/suspend"`) != wantSuspend || strings.Contains(html, `action="/sessions/unsuspend"`) != wantUnsuspend {
				t.Fatalf("incorrect controls suspended=%v permitted=%v", suspended, permitted)
			}
			if suspended && strings.Contains(html, `action="/sessions/detach"`) {
				t.Fatal("suspended row offers detach")
			}
			if wantSuspend && !strings.Contains(html, `data-confirm-action="session-suspend"`) {
				t.Fatal("missing queue discard confirmation")
			}
		}
	}
}

func TestDetachedSessionHasNoSuspendControl(t *testing.T) {
	v := page{View: "sessions", Project: "demo", Sessions: []Session{{ID: "one", Name: "detached", ErrorCode: "detached", Status: "missing"}}, CanManage: map[string]bool{"one": true}}
	v.User.Username = "owner"
	w := httptest.NewRecorder()
	if err := pages.ExecuteTemplate(w, "page", v); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"suspend", "unsuspend", "detach"} {
		if strings.Contains(w.Body.String(), `action="/sessions/`+action+`"`) {
			t.Fatal("detached session offered a connection management action")
		}
	}
}

type suspendBackend struct {
	panelBackend
	suspended bool
	fail      bool
	changes   int
}

type isolatedSuspendBackend struct {
	panelBackend
	suspended atomic.Bool
	lookup    func()
}

func (b *isolatedSuspendBackend) Sessions(context.Context, string) ([]Session, error) {
	return []Session{{ID: "paused-id", Name: "paused"}}, nil
}

func (b *isolatedSuspendBackend) SessionSuspended(_ context.Context, project, nickname string) (bool, error) {
	paused := b.suspended.Load()
	if b.lookup != nil {
		b.lookup()
	}
	return paused && project == "demo" && nickname == "paused", nil
}

func (b *isolatedSuspendBackend) SetSuspended(_ context.Context, _, _ string, paused bool) error {
	b.suspended.Store(paused)
	return nil
}

func TestSuspendCancelsOnlyMatchingInflightToken(t *testing.T) {
	s := userStore(t)
	paused, err := s.IssueToken("demo", "paused")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.IssueToken("demo", "other")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan string, 2)
	completed := make(chan string, 2)
	finishOther := make(chan struct{})
	b := &isolatedSuspendBackend{}
	h := New(s, b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grant, _ := access.FromContext(r.Context())
		started <- grant.Nickname
		if grant.Nickname == "other" {
			select {
			case <-r.Context().Done():
				completed <- "other-cancelled"
			case <-finishOther:
				completed <- "other-finished"
			}
		} else {
			<-r.Context().Done()
			completed <- "paused-cancelled"
		}
	}))
	owner := loginPanelUser(t, h, "owner")
	for _, token := range []string{paused, other} {
		r := httptest.NewRequest("POST", "https://broker/mcp?project=demo", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+token)
		go h.ServeHTTP(httptest.NewRecorder(), r)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("requests did not start")
		}
	}
	if w := owner.call("POST", "/sessions/suspend", url.Values{"project": {"demo"}, "id": {"paused-id"}}); w.Code != 303 {
		t.Fatalf("suspend = %d", w.Code)
	}
	select {
	case got := <-completed:
		if got != "paused-cancelled" {
			t.Fatalf("wrong token cancelled: %s", got)
		}
	case <-ctx.Done():
		t.Fatal("suspended request was not cancelled")
	}
	close(finishOther)
	select {
	case got := <-completed:
		if got != "other-finished" {
			t.Fatalf("unrelated token interrupted: %s", got)
		}
	case <-ctx.Done():
		t.Fatal("other request failed to finish")
	}
}

func TestSuspendRacingPolicyLookupReturns423(t *testing.T) {
	s := userStore(t)
	token, err := s.IssueToken("demo", "paused")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	looked, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	b := &isolatedSuspendBackend{lookup: func() {
		if calls.Add(1) == 1 {
			close(looked)
			<-release
		}
	}}
	var forwarded atomic.Int32
	h := New(s, b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(204) }))
	owner := loginPanelUser(t, h, "owner")
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://broker/mcp?project=demo", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request() }()
	select {
	case <-looked:
	case <-ctx.Done():
		t.Fatal("policy lookup did not start")
	}
	w := owner.call("POST", "/sessions/suspend", url.Values{"project": {"demo"}, "id": {"paused-id"}})
	close(release)
	if w.Code != 303 {
		t.Fatalf("suspend = %d", w.Code)
	}
	select {
	case got := <-done:
		if got.Code != 423 {
			t.Fatalf("suspend admission race = %d, want 423", got.Code)
		}
	case <-ctx.Done():
		t.Fatal("policy lookup did not finish")
	}
	if got := request(); got.Code != 423 {
		t.Fatalf("new suspended request = %d", got.Code)
	}
	if forwarded.Load() != 0 {
		t.Fatal("suspended request reached backend")
	}
	if w := owner.call("POST", "/sessions/unsuspend", url.Values{"project": {"demo"}, "id": {"paused-id"}}); w.Code != 303 {
		t.Fatalf("unsuspend = %d", w.Code)
	}
	if got := request(); got.Code != 204 {
		t.Fatalf("unsuspended request = %d", got.Code)
	}
}

func (b *suspendBackend) SessionSuspended(_ context.Context, project, nickname string) (bool, error) {
	if b.fail {
		return false, errors.New("unavailable")
	}
	return b.suspended && nickname == "paused", nil
}
func (b *suspendBackend) SetSuspended(_ context.Context, project, id string, suspended bool) error {
	b.suspended = suspended
	b.changes++
	return nil
}

func TestSuspendedMCPAdmission(t *testing.T) {
	s := userStore(t)
	paused, err := s.IssueToken("demo", "paused")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.IssueToken("demo", "other")
	if err != nil {
		t.Fatal(err)
	}
	b := &suspendBackend{suspended: true}
	forwarded := 0
	h := New(s, b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded++; w.WriteHeader(204) }))
	check := func(token string, want int) {
		t.Helper()
		r := httptest.NewRequest("POST", "https://broker/mcp?project=demo", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("status %d, want %d", w.Code, want)
		}
	}
	check(paused, 423)
	if forwarded != 0 {
		t.Fatal("suspended request reached MCP")
	}
	check(other, 204)
	b.suspended = false
	check(paused, 204)
	b.fail = true
	check(paused, 503)
}

func TestSuspendUIAuthorizationAndCSRF(t *testing.T) {
	s := userStore(t)
	b := &suspendBackend{}
	h := New(s, b, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	values := url.Values{"project": {"demo"}, "id": {"session-1"}}
	for _, path := range []string{"/sessions/suspend", "/sessions/unsuspend"} {
		w := owner.call("POST", path, values)
		if w.Code != 303 {
			t.Fatalf("%s = %d", path, w.Code)
		}
	}
	if b.changes != 2 || b.suspended {
		t.Fatal("management state not applied")
	}
	viewer := loginPanelUser(t, h, "viewer")
	if got := viewer.call("POST", "/sessions/suspend", values).Code; got != 403 {
		t.Fatalf("viewer = %d", got)
	}
	r := httptest.NewRequest("POST", "http://broker/sessions/suspend", nil)
	r.AddCookie(owner.cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || b.changes != 2 {
		t.Fatal("CSRF bypass")
	}
}
