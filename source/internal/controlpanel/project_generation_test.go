package controlpanel

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"testing"
)

func TestProjectGenerationRestoredAuditIsNotCurrent(t *testing.T) {
	s := userStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	event := AuditEvent{Version: 1, Project: "demo", OwnerID: "owner", Kind: "detached", At: s.now(), Management: &protocol.ManagementEvent{Kind: "detached", ActorName: "panel", TargetName: "old", TargetRegistrationID: "OLD", At: s.now()}}
	raw, _ := json.Marshal(event)
	// A generation-less old backup must never be assigned to a recreated name.
	idle, err := s.beginProjectDeletion("owner", "demo", "demo")
	if err != nil {
		t.Fatal(err)
	}
	<-idle
	if err = s.finishProjectDeletion("demo"); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProjectFor("owner", "demo"); err != nil {
		t.Fatal(err)
	}
	var restored AuditEvent
	json.Unmarshal(raw, &restored)
	s.audits = map[string]AuditEvent{"audit/restored": restored}
	events, err := s.DirectAuditFor("owner", "demo")
	if err != nil || len(events) != 0 {
		t.Fatal("restored old-generation audit is visible in the current project")
	}
}

func TestProjectGenerationUnknownSurvivesCleanupAndAdminOnly(t *testing.T) {
	s := userStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	now := s.now()
	unknown := AuditEvent{Version: 1, Project: "absent", OwnerID: "owner", Kind: "detached", At: now, Management: &protocol.ManagementEvent{Kind: "detached", ActorName: "panel", TargetName: "legacy", TargetRegistrationID: "OLD", At: now}}
	known := unknown
	known.Project = "demo"
	known.ProjectGenerationID = s.ProjectGeneration("demo")
	previous := known
	previous.ProjectGenerationID = "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	s.audits = map[string]AuditEvent{"audit/u": unknown, "audit/c": known, "audit/p": previous}
	if _, err := s.beginProjectDeletion("owner", "demo", "demo"); err != nil {
		t.Fatal(err)
	}
	if err := s.finishProjectDeletion("demo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.audits["audit/u"]; !ok {
		t.Fatal("orphan cleanup erased unknown")
	}
	if _, ok := s.audits["audit/c"]; ok {
		t.Fatal("current generation survived its deletion")
	}
	if _, ok := s.audits["audit/p"]; !ok {
		t.Fatal("deletion erased another generation")
	}
	for _, actor := range []string{"owner", "developer", "viewer", "outsider", ""} {
		if _, err := s.DirectAuditRecordsFor(actor, "", AuditFilter{Bucket: "history"}); err == nil {
			t.Fatal("non-admin history access")
		}
	}
	rows, err := s.DirectAuditRecordsFor("admin", "", AuditFilter{Bucket: "history"})
	if err != nil || len(rows) != 2 {
		t.Fatal("admin history lost previous or unknown")
	}
	// Ordinary subsequent commits keep unknown records, but still prune known orphans.
	if err = s.CreateProjectFor("owner", "new-project"); err != nil {
		t.Fatal(err)
	}
	if len(s.audits) != 1 || s.audits["audit/u"].Project != "absent" {
		t.Fatal("known-orphan cleanup or unknown preservation failed")
	}
}

func TestProjectGenerationRestoredKnownAuditAndReportArePrevious(t *testing.T) {
	s := userStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	reports, err := bugreport.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reports.Close()
	reports.SetProjectPolicy(s.BugReportPolicy)
	oldID := s.ProjectGeneration("demo")
	report, err := reports.Create(bugreport.Input{Title: "old backup", Summary: "fixture"}, bugreport.Reporter{ProjectGenerationID: oldID, Project: "demo", SessionName: "old", RegistrationID: "old", Harness: "codex", OwnerID: "owner"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	seedRound2Audit(t, s, "demo", "old")
	var old AuditEvent
	for _, e := range s.audits {
		old = e
	}
	if old.ProjectGenerationID != oldID {
		t.Fatal("management audit was not stamped")
	}
	idle, err := s.beginProjectDeletion("owner", "demo", "demo")
	if err != nil {
		t.Fatal(err)
	}
	<-idle
	if err = s.finishProjectDeletion("demo", oldID); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProjectFor("owner", "demo"); err != nil {
		t.Fatal(err)
	}
	if s.ProjectGeneration("demo") == oldID {
		t.Fatal("new project reused old identity")
	}
	// Report data represents an older backup; restore the old audit alongside it.
	s.mu.Lock()
	s.audits["audit/restored"] = old
	s.mu.Unlock()
	rows, err := s.DirectAuditFor("owner", "demo")
	if err != nil || len(rows) != 0 {
		t.Fatal("restored previous audit visible to new owner")
	}
	rows, err = s.DirectAuditRecordsFor("admin", "demo", AuditFilter{Bucket: "previous"})
	if err != nil || len(rows) != 1 {
		t.Fatal("admin previous audit missing")
	}
	if _, err = reports.Get(report.ID); err != bugreport.ErrNotFound {
		t.Fatal("restored report visible in recreated project")
	}
	if _, err = reports.GetFor("admin", report.ID, "previous"); err != nil {
		t.Fatal("admin previous report missing")
	}
	for _, actor := range []string{"owner", "developer", "viewer"} {
		if _, err = reports.GetFor(actor, report.ID, "previous"); err != bugreport.ErrForbidden {
			t.Fatal("non-admin historical detail")
		}
	}
	// Delayed events from the authenticated old context must never be restamped.
	now := s.now()
	if err = s.RecordSessionManagement("demo", "owner", protocol.ManagementEvent{Kind: "detached", ActorName: "old", TargetName: "old", TargetRegistrationID: "old", At: now}, oldID); err != ErrForbidden {
		t.Fatal("stale management event entered new generation")
	}
	if _, err = s.beginProjectDeletion("owner", "demo", "demo"); err != nil {
		t.Fatal(err)
	}
	if err = s.finishProjectDeletion("demo", oldID); err == nil {
		t.Fatal("stale deletion completed against new generation")
	}
}

func TestProjectGenerationStampsAllAuditKindsAndRejectsStaleStamp(t *testing.T) {
	s := userStore(t)
	now := s.now()
	events := []AuditEvent{
		auditFor("demo", AutoBinding{ParentID: strings.Repeat("P", 52), GrantID: strings.Repeat("G", 52), Identity: "fixture"}, "owner", "registration", "confirmed", now),
		{Version: 1, Project: "demo", OwnerID: "owner", Kind: "handover", At: now, Handover: &protocol.HandoverAudit{Previous: strings.Repeat("a", 64), Current: strings.Repeat("b", 64), RegistrationID: "REG", Name: "worker", At: now}},
		{Version: 1, Project: "demo", OwnerID: "owner", Kind: "suspended", At: now, Management: &protocol.ManagementEvent{Kind: "suspended", ActorName: "pm", TargetName: "worker", TargetRegistrationID: "REG", At: now}},
	}
	s.mu.Lock()
	err := s.commitAudited(s.data, events)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s.audits {
		if e.ProjectGenerationID != s.ProjectGeneration("demo") {
			t.Fatal("audit kind not stamped")
		}
	}
	if len(s.audits) != 3 {
		t.Fatal("audit kind dropped")
	}
	events = events[:1]
	events[0].ProjectGenerationID = "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	s.mu.Lock()
	err = s.commitAudited(s.data, events)
	s.mu.Unlock()
	if err == nil || len(s.audits) != 3 {
		t.Fatal("cross-generation event committed")
	}
}
