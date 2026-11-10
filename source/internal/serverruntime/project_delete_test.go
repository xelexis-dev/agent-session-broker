package serverruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

func TestManagedProjectDeleteClosesSQLiteAndRecreatesEmpty(t *testing.T) {
	root := t.TempDir()
	registry, err := groups.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	defer registry.Close()
	defer factory.Close()
	for _, name := range []string{"demo", "keep"} {
		service, err := factory.RemoteBackend(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.(broker.DurableRegistrar).RegisterSession(t.Context(), broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir(), ClientID: "pc-demo-01", Name: "registered", ChannelID: "lease-demo-01", Secret: strings.Repeat("s", 64), RequestID: "request-demo-01"})
		if err != nil || result.Code != "" {
			t.Fatalf("register: %s %v", result.Code, err)
		}
		p, err := registry.ProjectStorage(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(p.CanonicalPath(), "memory", "fixture.md"), []byte("shared memory fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	backend := panelBackend{factory: factory, registry: registry}
	if err = backend.DeleteProject(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	imports, err := existingProjects(root)
	if err != nil || len(imports) != 1 || imports[0] != "keep" {
		t.Fatalf("old directory imported: %v %v", imports, err)
	}
	fresh, err := factory.RemoteBackend(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := fresh.ListSessions(t.Context())
	if err != nil || len(sessions) != 0 {
		t.Fatal("registrations survived", err)
	}
	p, err := registry.ProjectStorage(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(p.CanonicalPath(), "memory", "fixture.md")); !os.IsNotExist(err) {
		t.Fatal("shared memory survived", err)
	}
	keep, err := backend.Sessions(t.Context(), "keep")
	if err != nil || len(keep) != 1 {
		t.Fatal("neighbor affected", err)
	}
	if err = factory.Close(); err != nil {
		t.Fatal(err)
	}
	if err = registry.Close(); err != nil {
		t.Fatal(err)
	}
	registry, err = groups.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory = mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	defer factory.Close()
	fresh, err = factory.RemoteBackend(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err = fresh.ListSessions(t.Context())
	if err != nil || len(sessions) != 0 {
		t.Fatal("restart resurrected registration", err)
	}
}

func TestProjectDeleteRemovesOnlyItsBugReports(t *testing.T) {
	root := t.TempDir()
	control, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err = control.Setup("synthetic-delete-password"); err != nil {
		t.Fatal(err)
	}
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	defer factory.Close()
	reports, err := bugreport.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reports.Close()
	for _, project := range []string{"demo", "keep"} {
		if err = control.CreateProject(project); err != nil {
			t.Fatal(err)
		}
		if _, err = factory.RemoteBackend(t.Context(), project); err != nil {
			t.Fatal(err)
		}
		if _, err = reports.Create(bugreport.Input{Title: "fixture", Summary: "fixture"}, bugreport.Reporter{ProjectGenerationID: control.ProjectGeneration(project), Project: project, SessionName: "worker", RegistrationID: "registration", Harness: "codex", OwnerID: "owner"}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	backend := panelBackend{factory: factory, registry: registry, store: control, bugreports: reports}
	if err = backend.DeleteProject(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	rows, total, err := reports.List(bugreport.Filter{}, 1)
	if err != nil || total != 1 || rows[0].Reporter.Project != "keep" {
		t.Fatal("project deletion affected the wrong bug reports")
	}
	if backend.BugReports() != reports {
		t.Fatal("panel report API store not wired")
	}
}
