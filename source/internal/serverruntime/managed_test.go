package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestExistingProjectsImportsOnlyCanonicalDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"p-617362", "p-74657374", "p-54657374", "p-2e2e2f657363617065", "random"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "p-66696c65"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	names, err := existingProjects(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "asb" || names[1] != "test" {
		t.Fatalf("imports: %v", names)
	}
}

func TestManagedRuntimeRequiresSetupBeforeProjectAllocation(t *testing.T) {
	calls := 0
	h, close, err := newManagedHandler(t.TempDir(), func(groups.ProjectDirectory) (mcpserver.Backend, error) {
		calls++
		return broker.NewRemoteService(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	for _, test := range []struct {
		path     string
		status   int
		contains string
	}{{"/healthz", 200, "ok"}, {"/setup", 200, "admin"}, {"/mcp?project=unknown", 503, "setup"}} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.contains) {
			t.Fatalf("%s: %d %s", test.path, response.Code, response.Body.String())
		}
	}
	if calls != 0 {
		t.Fatal("unauthenticated request allocated backend")
	}
}

func TestPanelBackendUsesProjectService(t *testing.T) {
	r, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	factory := mcpserver.NewRemoteProjectFactory(r)
	defer factory.Close()
	service, err := factory.RemoteBackend(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	remote := service.(*broker.RemoteService)
	id := "00000000-0000-4000-8000-000000000001"
	_, err = remote.AttachSession(context.Background(), broker.AttachRequest{Name: "example", Harness: "codex", SessionID: id, PID: 123, CWD: t.TempDir(), ChannelID: "lease"})
	if err != nil {
		t.Fatal(err)
	}
	adapter := panelBackend{factory: factory}
	sessions, err := adapter.Sessions(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "example" || !sessions[0].Ready {
		t.Fatalf("sessions: %+v", sessions)
	}
	if err := adapter.Detach(context.Background(), "test", id); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.GetSession(context.Background(), id); !errors.Is(err, broker.ErrSessionNotFound) {
		t.Fatal("UI did not remove the detached registration", err)
	}
	sessions, err = adapter.Sessions(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatal("panel still lists the detached registration")
	}
}

type panelAssignedName string

func (name panelAssignedName) Admit(context.Context, broker.RegisterRequest) (string, string, error) {
	return string(name), "", nil
}

func panelCollisionFixture(t *testing.T) (panelBackend, *broker.RemoteService, broker.Session) {
	t.Helper()
	r, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	factory := mcpserver.NewRemoteProjectFactory(r)
	t.Cleanup(func() { factory.Close() })
	service, err := factory.RemoteBackend(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	remote := service.(*broker.RemoteService)
	const id = "00000000-0000-4000-8000-000000000001"
	if _, err := remote.AttachSession(t.Context(), broker.AttachRequest{Name: "legacy", Harness: "codex", SessionID: id, PID: 123, CWD: t.TempDir(), ChannelID: "legacy-lease"}); err != nil {
		t.Fatal(err)
	}
	remote.SetAdmission(panelAssignedName(id))
	other := protectedRegistered(t, remote, protectedRequest(2, "other"))
	remote.SetAdmission(nil)
	return panelBackend{factory: factory}, remote, other
}

func TestPanelDetachLegacyIDDoesNotSelectAnotherNickname(t *testing.T) {
	// A token policy can assign a UUID-shaped nickname to another registration.
	adapter, remote, other := panelCollisionFixture(t)
	rows, err := adapter.Sessions(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	selected := ""
	for _, row := range rows {
		if row.Name == "legacy" {
			selected = row.ID
		}
	}
	if selected == "" {
		t.Fatal("legacy UI row missing")
	}
	if err := adapter.Detach(t.Context(), "test", selected); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.GetSession(t.Context(), broker.SelectorName+"legacy"); !errors.Is(err, broker.ErrSessionNotFound) {
		t.Fatal("intended legacy registration was not removed", err)
	}
	if got, err := remote.GetSession(t.Context(), broker.SelectorRegistration+other.RegistrationID); err != nil || !got.AutomaticDeliveryReady {
		t.Fatal("nickname holder was incorrectly detached", err)
	}
}

func TestPanelDetachSelectorsKeepNamespacesAndExistingIDs(t *testing.T) {
	const legacyID = "00000000-0000-4000-8000-000000000001"
	for _, test := range []struct {
		name, selector        string
		removeLegacy, missing bool
	}{
		{"explicit name", "name:" + legacyID, false, false},
		{"explicit session", "session:" + legacyID, true, false},
		{"explicit registration", "registration:$other", false, false},
		{"existing registration id", "$other", false, false},
		{"existing name", "legacy", true, false},
		{"unique durable session id", "11111111-1111-4111-8111-000000000002", false, false},
		{"registration namespace miss", "registration:" + legacyID, false, true},
		{"session namespace miss", "session:$other", false, true},
		{"name namespace miss", "name:$other", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, remote, other := panelCollisionFixture(t)
			selector := strings.ReplaceAll(test.selector, "$other", other.RegistrationID)
			err := adapter.Detach(t.Context(), "test", selector)
			if test.missing {
				if !errors.Is(err, broker.ErrSessionNotFound) {
					t.Fatalf("typed miss = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			legacy, legacyErr := remote.GetSession(t.Context(), "name:legacy")
			remaining, otherErr := remote.GetSession(t.Context(), "registration:"+other.RegistrationID)
			if !test.missing && test.removeLegacy {
				if !errors.Is(legacyErr, broker.ErrSessionNotFound) || otherErr != nil || !remaining.AutomaticDeliveryReady {
					t.Fatal("legacy selector affected the wrong registration")
				}
			} else if !test.missing {
				if !errors.Is(otherErr, broker.ErrSessionNotFound) || legacyErr != nil || !legacy.AutomaticDeliveryReady {
					t.Fatal("nickname/registration selector affected the wrong session")
				}
			} else if legacyErr != nil || otherErr != nil || !legacy.AutomaticDeliveryReady || !remaining.AutomaticDeliveryReady {
				t.Fatal("typed miss detached a session in another namespace")
			}
		})
	}
}

func TestPanelDetachLegacyRowRemainsDistinctFromDurableSameConversation(t *testing.T) {
	adapter, remote, other := panelCollisionFixture(t)
	const legacyID = "00000000-0000-4000-8000-000000000001"
	request := protectedRequest(3, "same-conversation")
	request.SessionID = legacyID
	sameConversation := protectedRegistered(t, remote, request)
	if err := adapter.Detach(t.Context(), "test", "session:"+legacyID); !errors.Is(err, broker.ErrSessionNotFound) {
		t.Fatal("ambiguous session selector did not fail safely", err)
	}
	if got, err := remote.GetSession(t.Context(), "name:legacy"); err != nil || !got.AutomaticDeliveryReady {
		t.Fatal("ambiguous selector changed the legacy row", err)
	}
	if err := adapter.Detach(t.Context(), "test", legacyID); err != nil {
		t.Fatal("existing legacy UI id failed with a durable same-conversation row", err)
	}
	if _, err := remote.GetSession(t.Context(), "name:legacy"); !errors.Is(err, broker.ErrSessionNotFound) {
		t.Fatal("legacy row was not removed", err)
	}
	for _, id := range []string{other.RegistrationID, sameConversation.RegistrationID} {
		if got, err := remote.GetSession(t.Context(), "registration:"+id); err != nil || !got.AutomaticDeliveryReady {
			t.Fatal("legacy detach affected a durable registration", err)
		}
	}
}

func TestPanelDetachDistinguishesSameConversationOnTwoPCs(t *testing.T) {
	r, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	factory := mcpserver.NewManagedProjectFactory(r, persistentBackend)
	defer factory.Close()
	service, err := factory.RemoteBackend(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	registrar := service.(broker.DurableRegistrar)
	for _, pc := range []string{"pc-alpha-01", "pc-beta-002"} {
		result, err := registrar.RegisterSession(t.Context(), broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir(), ClientID: pc, Name: pc, ChannelID: pc, Secret: strings.Repeat(pc, 4), RequestID: pc})
		if err != nil || result.Code != "" {
			t.Fatalf("register: %s %v", result.Code, err)
		}
	}
	adapter := panelBackend{factory: factory}
	sessions, err := adapter.Sessions(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID == sessions[1].ID {
		t.Fatalf("panel cannot distinguish PCs: %+v", sessions)
	}
	if err := adapter.Detach(t.Context(), "test", sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	_, removedErr := service.GetSession(t.Context(), sessions[0].Name)
	b, _ := service.GetSession(t.Context(), sessions[1].Name)
	if !errors.Is(removedErr, broker.ErrSessionNotFound) || !b.AutomaticDeliveryReady {
		t.Fatal("UI detached the wrong PC")
	}
}

type failedCleanupBackend struct{ *broker.RemoteService }

func (b failedCleanupBackend) RemoveDetachedSession(context.Context, string, func(broker.Session) error) error {
	return errors.New("registration storage unavailable")
}

func (b failedCleanupBackend) RemoveDetachedSessions(context.Context, []string) ([]string, error) {
	return nil, errors.New("registration storage unavailable")
}

func TestPanelCleanupFailureDoesNotBlockOtherSessionManagement(t *testing.T) {
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	remote := broker.NewRemoteService()
	factory := mcpserver.NewManagedProjectFactory(registry, func(groups.ProjectDirectory) (mcpserver.Backend, error) {
		return failedCleanupBackend{remote}, nil
	})
	defer factory.Close()
	if _, err := factory.RemoteBackend(t.Context(), "test"); err != nil {
		t.Fatal(err)
	}
	first := protectedRegistered(t, remote, protectedRequest(1, "detached"))
	second := protectedRegistered(t, remote, protectedRequest(2, "active"))
	if err := remote.StopSession(t.Context(), first.RegistrationID); err != nil {
		t.Fatal(err)
	}
	adapter := panelBackend{factory: factory}
	sessions, err := adapter.Sessions(t.Context(), "test")
	if err != nil || len(sessions) != 2 {
		t.Fatal("one failed cleanup suppressed the session list", err)
	}
	if err := adapter.SetSuspended(t.Context(), "test", second.RegistrationID, true); err != nil {
		t.Fatal("other session cannot be managed", err)
	}
	if got, err := remote.GetSession(t.Context(), second.RegistrationID); err != nil || !got.Suspended {
		t.Fatal("other session management failed")
	}
}

func TestPanelLiveSessionsDoesNotCleanDetachedRegistration(t *testing.T) {
	adapter, remote, registered := panelCollisionFixture(t)
	if err := remote.StopSession(t.Context(), registered.RegistrationID); err != nil {
		t.Fatal(err)
	}
	rows, err := adapter.LiveSessions(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.RegistrationID == registered.RegistrationID {
			found = true
			if row.ErrorCode != "detached" {
				t.Fatalf("detached row=%+v", row)
			}
		}
	}
	if !found {
		t.Fatal("live read removed detached row")
	}
	if _, err := remote.GetSession(t.Context(), registered.RegistrationID); err != nil {
		t.Fatal("live read changed registration", err)
	}
}

func TestPanelLiveSessionsPassesActivitySnapshot(t *testing.T) {
	adapter, remote, registered := panelCollisionFixture(t)
	now := time.Now().UTC()
	raw, err := json.Marshal(protocol.Activity{State: protocol.ActivityWorking, Source: "codex-rollout", Sequence: 1, ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if renewed, err := remote.RenewLease(t.Context(), broker.RenewRequest{RegistrationID: registered.RegistrationID, ChannelID: protectedRequest(2, "other").ChannelID, Activity: raw}); err != nil || renewed.Code != "" {
		t.Fatalf("renew=%+v err=%v", renewed, err)
	}
	rows, err := adapter.LiveSessions(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.RegistrationID == registered.RegistrationID {
			if row.Activity == nil || row.Activity.State != protocol.ActivityWorking || row.Activity.ReceivedAt.IsZero() {
				t.Fatalf("activity not passed: %+v", row.Activity)
			}
			return
		}
	}
	t.Fatal("registered row missing")
}
