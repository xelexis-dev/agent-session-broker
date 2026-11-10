package serverruntime

import (
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

func detachAuditKinds(t *testing.T, s *controlpanel.Store, project string) map[string]int {
	t.Helper()
	events, err := s.DirectAuditFor("admin", project)
	if err != nil {
		t.Fatal("audit lookup")
	}
	counts := map[string]int{}
	for _, e := range events {
		switch e.Kind {
		case "detached", "revoked":
		default:
			continue
		}
		key := e.Kind
		if e.Kind == "revoked" {
			switch e.Reason {
			case "superseded", "explicit-release":
				key += "/" + e.Reason
			default:
				t.Fatal("unexpected revocation reason")
			}
		}
		counts[key]++
	}
	return counts
}

func TestPanelDetachAuditFacts(t *testing.T) {
	t.Run("durable-auto", func(t *testing.T) {
		r, parent := unregRuntime(t)
		_, out := unregAuto(t, r, parent, 1951, "detach-audit", true)
		response := r.browser(http.MethodPost, "/sessions/detach", url.Values{"project": {"test"}, "id": {out.Session.RegistrationID}})
		if response.Code != http.StatusSeeOther {
			t.Fatal("panel detach failed")
		}
		if err := r.close(); err != nil {
			t.Fatal("runtime close")
		}
		s, err := controlpanel.OpenSQLite(filepath.Join(r.dir, "control"))
		if err != nil {
			t.Fatal("audit open")
		}
		defer s.Close()
		counts := detachAuditKinds(t, s, "test")
		t.Logf("panel detach audit kinds=%v", counts)
		if counts["detached"] != 1 {
			t.Fatal("panel management fact missing or duplicated")
		}
		if counts["revoked/explicit-release"] != 1 || counts["revoked/superseded"] != 0 {
			t.Fatal("one panel detach must record exactly one explicit revocation")
		}
	})
	t.Run("legacy", func(t *testing.T) {
		s, err := controlpanel.OpenSQLite(t.TempDir())
		if err != nil {
			t.Fatal("store")
		}
		defer s.Close()
		if s.Setup("test-only-audit-password") != nil || s.CreateProject("test") != nil || s.SetScopeIssuanceReady("admin", true) != nil {
			t.Fatal("setup")
		}
		registry, err := groups.NewRegistry(t.TempDir())
		if err != nil {
			t.Fatal("registry")
		}
		defer registry.Close()
		factory := mcpserver.NewRemoteProjectFactory(registry)
		defer factory.Close()
		service, err := factory.RemoteBackend(t.Context(), "test")
		if err != nil {
			t.Fatal("backend")
		}
		legacy, err := service.(*broker.RemoteService).AttachSession(t.Context(), broker.AttachRequest{Name: "legacy-audit", Harness: "codex", SessionID: "11111111-1111-4111-8111-000000001952", PID: 1952, CWD: "/synthetic/audit", ChannelID: "legacy-audit-lease"})
		if err != nil {
			t.Fatal("legacy attach")
		}
		adapter := panelBackend{factory: factory, store: s}
		h := controlpanel.New(s, adapter, http.NotFoundHandler())
		r := &deviceRuntime{t: t, h: h, project: "test"}
		r.login("test-only-audit-password")
		response := r.browser(http.MethodPost, "/sessions/detach", url.Values{"project": {"test"}, "id": {legacy.ID}})
		if response.Code != http.StatusSeeOther {
			t.Fatal("legacy panel detach failed")
		}
		counts := detachAuditKinds(t, s, "test")
		t.Logf("legacy panel detach audit kinds=%v", counts)
		if len(counts) != 0 {
			t.Fatal("legacy detach unexpectedly emitted durable management or auto-grant audit")
		}
	})
}
