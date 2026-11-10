package serverruntime

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"strings"
	"testing"
	"time"
)

func TestAnnouncementAcrossProjectsAndPartialFailures(t *testing.T) {
	reg, e := groups.NewRegistry(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer reg.Close()
	factory := mcpserver.NewRemoteProjectFactory(reg)
	defer factory.Close()
	store, e := controlpanel.OpenSQLite(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if e = store.Setup("local-test-password"); e != nil {
		t.Fatal(e)
	}
	remotes := map[string]*broker.RemoteService{}
	for _, name := range []string{"one", "two"} {
		if e = store.CreateProject(name); e != nil {
			t.Fatal(e)
		}
		svc, e := factory.RemoteBackend(t.Context(), name)
		if e != nil {
			t.Fatal(e)
		}
		s := svc.(*broker.RemoteService)
		remotes[name] = s
		for i, n := range []string{"ready", "full", "missing"} {
			id := broker.AttachRequest{Name: n, Harness: "codex", SessionID: []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}[i], PID: 123, CWD: t.TempDir(), ChannelID: n}
			if _, e = s.AttachSession(t.Context(), id); e != nil {
				t.Fatal(e)
			}
		}
		for i := 0; i < 32; i++ {
			if _, e = s.SendMessage(t.Context(), broker.Message{From: "ready", To: "full", Body: "existing"}); e != nil {
				t.Fatal(e)
			}
		}
		if e = s.ReportIngressFailure(t.Context(), "missing", "missing"); e != nil {
			t.Fatal(e)
		}
	}
	b := panelBackend{factory: factory, registry: reg, store: store}
	announcer, ok := any(b).(interface {
		Announce(context.Context, string, string, string) ([]controlpanel.AnnouncementResult, error)
	})
	if !ok {
		t.Fatal("admin announcement orchestration missing")
	}
	results, e := announcer.Announce(t.Context(), "admin", "", "notice")
	if e != nil || len(results) != 6 {
		t.Fatalf("results %+v %v", results, e)
	}
	accepted := 0
	for _, r := range results {
		if r.Accepted {
			accepted++
		} else if r.Error == "" {
			t.Fatal("missing failure reason", r)
		}
	}
	if accepted != 2 {
		t.Fatal("partial failures stopped other recipients", results)
	}
	for _, s := range remotes {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		m, e := s.WaitForIngress(ctx, "ready", "ready")
		cancel()
		if e != nil || m.From != "broker-admin" || m.Body != "notice" {
			t.Fatal(m, e)
		}
	}
	results, e = announcer.Announce(t.Context(), "admin", "one", "project notice")
	if e != nil || len(results) != 3 {
		t.Fatal(results, e)
	}
	if other, e := remotes["two"].GetSession(t.Context(), "ready"); e != nil || other.PendingMessages != 0 {
		t.Fatal("project scope leaked", other, e)
	}
	if _, e = announcer.Announce(t.Context(), "admin", "", strings.Repeat("x", 12000)); e == nil {
		t.Fatal("oversize accepted")
	}
	if _, e = announcer.Announce(t.Context(), "unknown", "", "notice"); e == nil {
		t.Fatal("nonadmin orchestration accepted")
	}
}
