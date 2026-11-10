package serverruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

type suspensionLookupBackend struct {
	*broker.RemoteService
	lists, gets int
	selector    string
}

func (b *suspensionLookupBackend) ListSessions(ctx context.Context) ([]broker.Session, error) {
	b.lists++
	return b.RemoteService.ListSessions(ctx)
}
func (b *suspensionLookupBackend) GetSession(ctx context.Context, selector string) (broker.Session, error) {
	b.gets++
	b.selector = selector
	return b.RemoteService.GetSession(ctx, selector)
}

func suspensionLookupFixture(t *testing.T) (panelBackend, *suspensionLookupBackend) {
	t.Helper()
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	backend := &suspensionLookupBackend{RemoteService: broker.NewRemoteService()}
	factory := mcpserver.NewManagedProjectFactory(registry, func(groups.ProjectDirectory) (mcpserver.Backend, error) { return backend, nil })
	t.Cleanup(func() { _ = factory.Close() })
	return panelBackend{factory: factory}, backend
}
func TestSessionSuspensionLookupAvoidsRoster(t *testing.T) {
	adapter, backend := suspensionLookupFixture(t)
	result, err := backend.RegisterSession(t.Context(), protectedRequest(1, "target"))
	if err != nil || result.Code != "" {
		t.Fatal("register target")
	}
	if err = backend.SetSuspended(t.Context(), result.Session.RegistrationID, true); err != nil {
		t.Fatal(err)
	}
	suspended, err := adapter.SessionSuspended(t.Context(), "project", "target")
	if err != nil || !suspended {
		t.Fatal("target suspension lost")
	}
	if backend.lists != 0 {
		t.Fatalf("suspension lookup built full roster %d times", backend.lists)
	}
	if backend.gets != 1 || backend.selector != broker.SelectorName+"target" {
		t.Fatal("suspension lookup did not address exactly the named target")
	}
}
func TestSessionSuspensionLookupPreservesNameNamespaceAndLifecycle(t *testing.T) {
	adapter, backend := suspensionLookupFixture(t)
	first, err := backend.RegisterSession(t.Context(), protectedRequest(1, "target"))
	if err != nil || first.Code != "" {
		t.Fatal("register target")
	}
	if err = backend.SetSuspended(t.Context(), first.Session.RegistrationID, true); err != nil {
		t.Fatal(err)
	}
	// Registration IDs are not names; UUID conversation IDs may be assigned names.
	if suspended, err := adapter.SessionSuspended(t.Context(), "project", first.Session.RegistrationID); err != nil || suspended {
		t.Fatal("registration ID was interpreted as a nickname")
	}
	backend.SetAdmission(panelAssignedName(first.Session.ID))
	second, err := backend.RegisterSession(t.Context(), protectedRequest(2, "other"))
	backend.SetAdmission(nil)
	if err != nil || second.Code != "" {
		t.Fatal("register UUID-shaped nickname")
	}
	if suspended, err := adapter.SessionSuspended(t.Context(), "project", first.Session.ID); err != nil || suspended {
		t.Fatal("UUID-shaped nickname resolved to the other registration")
	}
	if err = backend.SetSuspended(t.Context(), first.Session.RegistrationID, false); err != nil {
		t.Fatal(err)
	}
	if suspended, err := adapter.SessionSuspended(t.Context(), "project", "target"); err != nil || suspended {
		t.Fatal("missing lease after unsuspend retained suspension")
	}
	if err = backend.StopSession(t.Context(), first.Session.RegistrationID); err != nil {
		t.Fatal(err)
	}
	if suspended, err := adapter.SessionSuspended(t.Context(), "project", "target"); err != nil || suspended {
		t.Fatal("detached row retained suspension")
	}
	if err = backend.RemoveDetachedSession(t.Context(), first.Session.RegistrationID, nil); err != nil {
		t.Fatal(err)
	}
	if suspended, err := adapter.SessionSuspended(t.Context(), "project", "target"); err != nil || suspended {
		t.Fatal("absent registration should not reject first registration")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = adapter.SessionSuspended(ctx, "project", "target"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
func TestSessionSuspensionLookupRetainsScopeCheck(t *testing.T) {
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	defer factory.Close()
	adapter := panelBackend{factory: factory}
	ctx := access.WithGrant(t.Context(), access.Grant{Project: "project", Scopes: []string{access.SessionRegister}})
	if _, err := adapter.SessionSuspended(ctx, "project", "missing"); err == nil {
		t.Fatal("lookup bypassed existing session:read requirement")
	}
}
