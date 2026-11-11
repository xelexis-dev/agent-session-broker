package controlpanel

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"testing"
	"time"
)

func TestSessionManagementAuditProjectionAndReload(t *testing.T) {
	s := directStore(t)
	for _, tc := range []struct{ kind, actor, label string }{{"detached", "manager", "연결 해제"}, {"suspended", "panel", "일시 중지"}, {"unsuspended", "manager", "재개"}} {
		e := protocol.ManagementEvent{Kind: tc.kind, ActorName: tc.actor, ActorRegistrationID: "actor-registration", TargetName: "target-before-rename", TargetRegistrationID: "target-registration", TargetParentLabel: "target-parent-label", At: time.Now().UTC()}
		if err := s.RecordSessionManagement("demo", "admin", e); err != nil {
			t.Fatal(err)
		}
	}
	view, err := s.directAuditViewFor("admin", "demo", "en")
	if err != nil || len(view.Rows) != 3 {
		t.Fatal("audit projection", err)
	}
	for _, row := range view.Rows {
		if row.Name != "target-before-rename" || row.Registration != "target-registration" || row.Parent != "target-parent-label" || strings.Contains(row.Reason, "actor-registration") {
			t.Fatal("target-oriented row incorrect")
		}
		wantActor := "manager"
		if row.Kind == "suspended" {
			wantActor = en("audit.panel")
		}
		if row.Reason != en("audit.by", "actor", wantActor) {
			t.Fatal("actor reason missing")
		}
		if row.Event != map[string]string{"detached": en("audit.event.detached"), "suspended": en("audit.event.suspended"), "unsuspended": en("audit.event.unsuspended")}[row.Kind] {
			t.Fatal("kind label missing")
		}
	}
	root := s.root.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := reopened.DirectAuditFor("admin", "demo")
	if err != nil || len(events) != 3 {
		t.Fatal("persisted events missing", err)
	}
	for _, e := range events {
		raw, _ := json.Marshal(e)
		if e.Binding != "" || e.ParentID != "" || e.GrantID != "" || strings.Contains(string(raw), `"identity"`) {
			t.Fatal("management audit contains identity/token references")
		}
	}
	reopened.data.ScopeIssuanceReady = false
	gated, err := reopened.directAuditViewFor("admin", "demo", "en")
	if err != nil || gated.Enabled || len(gated.Rows) != 0 {
		t.Fatal("audit enable gate changed")
	}
}
