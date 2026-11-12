package serverruntime

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

var ownerCSRFPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

type failingOwnerBackfill struct {
	*protectedBackend
	calls     atomic.Int32
	failUntil int32
	retryGate <-chan struct{}
}

func (b *failingOwnerBackfill) BackfillRegistrationOwners(bindings map[string]string) error {
	if b.calls.Add(1) <= b.failUntil {
		return errors.New("fixture identity/path must never appear in logs")
	}
	if b.retryGate != nil {
		<-b.retryGate
	}
	return b.protectedBackend.BackfillRegistrationOwners(bindings)
}

func backfillFailureFixture(t *testing.T, failUntil int32) (string, *failingOwnerBackfill) {
	t.Helper()
	root := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Setup("fixture-password-long-enough"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProject("project", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	req := protectedRequest(1, "owned")
	grant := ownerRuntimeGrant(t, store, "admin")
	if err := grant.Claim(bindingIdentity(req.Harness, req.SessionID, req.ClientID)); err != nil {
		t.Fatal(err)
	}
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	service, err := factory.RemoteBackend(t.Context(), "project")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.(broker.DurableRegistrar).RegisterSession(t.Context(), req)
	if err != nil || result.Code != "" {
		t.Fatal("legacy seed failed", err, result.Code)
	}
	factory.Close()
	registry.Close()
	store.Close()
	return root, &failingOwnerBackfill{failUntil: failUntil}
}

func TestOwnerBackfillFailureKeepsStartupListAndDetachAvailable(t *testing.T) {
	output := captureDiagnostics(t)
	root, failed := backfillFailureFixture(t, 2)
	h, closeHandler, err := newManagedHandler(root, func(dir groups.ProjectDirectory) (mcpserver.Backend, error) {
		backend, err := persistentBackend(dir)
		if err != nil {
			return nil, err
		}
		failed.protectedBackend = backend.(*protectedBackend)
		return failed, nil
	})
	if err != nil {
		t.Fatalf("backfill failure blocked startup: %v", err)
	}
	defer closeHandler()
	rows, err := failed.ListSessions(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatal("list unavailable", err)
	}
	id := rows[0].RegistrationID
	if !strings.Contains(output.String(), `"type":"registration-owner-backfill-failed"`) || !strings.Contains(output.String(), `"count":1`) || strings.Contains(output.String(), "fixture identity/path") {
		t.Fatal("migration log must contain only fixed reason and count")
	}
	if failed.RegistrationOwners()[id] != "" {
		t.Fatal("failed backfill published owner")
	}
	// Exercise actual admin panel list/detach despite the failed migration.
	runtime := &deviceRuntime{t: t, h: h, project: "project", password: "fixture-password-long-enough"}
	runtime.login(runtime.password)
	page := runtime.browser("GET", "/projects/sessions?project=project", nil)
	if page.Code != http.StatusOK {
		t.Fatal("panel list unavailable", page.Code)
	}
	runtime.csrf = csrfFromOwnerPage(t, page)
	detached := runtime.browser("POST", "/sessions/detach", url.Values{"project": {"project"}, "id": {id}})
	if detached.Code != http.StatusSeeOther {
		t.Fatal("panel detach unavailable", detached.Code)
	}
}

func csrfFromOwnerPage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	match := ownerCSRFPattern.FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("page missing CSRF")
	}
	return match[1]
}

func TestOwnerBackfillLaterRetryPersistsOwner(t *testing.T) {
	shortOwnerBackfillRetry(t)
	root, failed := backfillFailureFixture(t, 1)
	// The retry may fire before startup returns under -race. Hold its write
	// until the first failed-write state has been asserted, then let it apply.
	gate := make(chan struct{})
	failed.retryGate = gate
	releaseRetry := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(releaseRetry)
	_, closeHandler, err := newManagedHandler(root, func(dir groups.ProjectDirectory) (mcpserver.Backend, error) {
		backend, err := persistentBackend(dir)
		if err != nil {
			return nil, err
		}
		failed.protectedBackend = backend.(*protectedBackend)
		return failed, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { releaseRetry(); closeHandler() }()
	rows, err := failed.ListSessions(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatal("fixture list failed", err)
	}
	id := rows[0].RegistrationID
	if failed.RegistrationOwners()[id] != "" {
		t.Fatal("first failed write published owner")
	}
	releaseRetry()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for failed.RegistrationOwners()[id] == "" {
		select {
		case <-deadline.C:
			t.Fatal("delayed backfill did not recover")
		case <-tick.C:
		}
	}
	if failed.calls.Load() != 2 {
		t.Fatal("retry count is not exactly one")
	}
	if err := closeHandler(); err != nil {
		t.Fatal(err)
	}
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	defer factory.Close()
	service, err := factory.RemoteBackend(t.Context(), "project")
	if err != nil {
		t.Fatal(err)
	}
	if service.(*protectedBackend).RegistrationOwners()[id] != "admin" {
		t.Fatal("retry owner did not survive restart")
	}
}

type retryOwnerFixture struct{ calls int }

func (f *retryOwnerFixture) BackfillRegistrationOwners(map[string]string) error {
	f.calls++
	return errors.New("retry fixture")
}
func TestOwnerBackfillRetriesOnlyOnceAndCancelsOnClose(t *testing.T) {
	shortOwnerBackfillRetry(t)
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "one retry", true: "cancel"}[cancel], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := &retryOwnerFixture{}
				retries := newOwnerBackfillRetries()
				defer retries.close()
				retries.run(fixture, nil)
				if cancel {
					retries.close()
				}
				time.Sleep(3 * ownerBackfillRetryDelay)
				synctest.Wait()
				want := 2
				if cancel {
					want = 1
				}
				if fixture.calls != want {
					t.Fatalf("backfill calls=%d, want%d", fixture.calls, want)
				}
			})
		})
	}
}

func TestOwnerBackfillDoesNotHideNormalBackendOpenFailure(t *testing.T) {
	root, _ := backfillFailureFixture(t, 1)
	_, _, err := newManagedHandler(root, func(groups.ProjectDirectory) (mcpserver.Backend, error) {
		return nil, errors.New("normal backend storage failure")
	})
	if err == nil {
		t.Fatal("normal storage failure became non-fatal")
	}
}

// These tests are serial; closeHandler/retries.close joins the retry goroutine
// before Cleanup restores the production delay.
func shortOwnerBackfillRetry(t *testing.T) {
	previous := ownerBackfillRetryDelay
	ownerBackfillRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { ownerBackfillRetryDelay = previous })
}
