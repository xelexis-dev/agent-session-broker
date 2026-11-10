package serverruntime

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"testing"
)

func TestPanelSuspendUsesExactProjectRegistration(t *testing.T) {
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	defer factory.Close()
	adapter := panelBackend{factory: factory}
	var first string
	for i, project := range []string{"one", "two"} {
		backend, err := factory.RemoteBackend(t.Context(), project)
		if err != nil {
			t.Fatal(err)
		}
		r := protectedRequest(i+1, "same-name")
		result, err := backend.(broker.DurableRegistrar).RegisterSession(t.Context(), r)
		if err != nil || result.Code != "" {
			t.Fatalf("register %s: %s %v", project, result.Code, err)
		}
		if project == "one" {
			first = result.Session.RegistrationID
		}
	}
	if err := adapter.SetSuspended(t.Context(), "two", first, true); err == nil {
		t.Fatal("cross-project suspension")
	}
	if err := adapter.SetSuspended(t.Context(), "one", first, true); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"one", "two"} {
		got, err := adapter.SessionSuspended(t.Context(), project, "same-name")
		if err != nil || got != (project == "one") {
			t.Fatalf("%s policy = %v %v", project, got, err)
		}
	}
	rows, err := adapter.Sessions(t.Context(), "one")
	if err != nil || len(rows) != 1 || !rows[0].Suspended || rows[0].ErrorCode != "suspended" || rows[0].Ready {
		t.Fatalf("DTO = %+v %v", rows, err)
	}
	if err := adapter.SetSuspended(t.Context(), "one", first, false); err != nil {
		t.Fatal(err)
	}
	rows, _ = adapter.Sessions(t.Context(), "one")
	if rows[0].Ready || rows[0].Suspended {
		t.Fatal("unsuspend invented liveness")
	}
}
