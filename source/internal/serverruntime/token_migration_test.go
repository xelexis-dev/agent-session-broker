package serverruntime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

func TestTokenOnlyUpgradePreservesLegacyRegistrationAndMemory(t *testing.T) {
	root := t.TempDir()
	control := filepath.Join(root, "control")
	seed := t.TempDir()
	store, err := controlpanel.OpenSQLite(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Setup("migration-fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateProject("test"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	// Represent an actual pre-token-only control file, independent of the
	// current public API which must not be able to create an open project.
	legacyPath := filepath.Join(control, "control.json")
	data := readControlFixture(t, seed)
	var legacy map[string]any
	if err = json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["disableUnauthenticated"] = false
	legacy["projects"].(map[string]any)["test"].(map[string]any)["mode"] = "open"
	data, _ = json.Marshal(legacy)
	if err = os.MkdirAll(control, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(legacyPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	backend, err := factory.RemoteBackend(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	request := protectedRequest(237, "old-name")
	registered, err := backend.(broker.DurableRegistrar).RegisterSession(t.Context(), request)
	if err != nil || registered.Code != "" {
		t.Fatal("could not seed legacy registration", err)
	}
	directory, err := registry.ProjectStorage(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	memoryPath := filepath.Join(directory.CanonicalPath(), "memory", "existing.md")
	const memory = "existing shared memory must remain byte-for-byte"
	if err = os.WriteFile(memoryPath, []byte(memory), 0600); err != nil {
		t.Fatal(err)
	}
	if err = factory.Close(); err != nil {
		t.Fatal(err)
	}
	registry.Close()
	for attempt := 0; attempt < 2; attempt++ {
		h, close, err := newManagedHandler(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://broker/mcp?project=test", nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("legacy open project accepted anonymous access: %d", response.Code)
		}
		if err = close(); err != nil {
			t.Fatal(err)
		}
	}
	migrated, err := controlpanel.OpenSQLite(control)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	projects := migrated.Projects()
	if len(projects) != 1 || projects[0].Mode != "token" || projects[0].Members["admin"] != "owner" {
		t.Fatal("project or membership lost during token migration")
	}
	registry, err = groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory = mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	defer factory.Close()
	backend, err = factory.RemoteBackend(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	old, err := backend.GetSession(t.Context(), registered.Session.RegistrationID)
	if err != nil || old.RegistrationID != registered.Session.RegistrationID || old.Name != request.Name || old.Status != broker.StatusMissing || old.AutomaticDeliveryReady {
		t.Fatalf("legacy registration was lost or resurrected: %+v %v", old, err)
	}
	kept, err := os.ReadFile(memoryPath)
	if err != nil || string(kept) != memory {
		t.Fatal("shared memory changed during token migration")
	}
}
