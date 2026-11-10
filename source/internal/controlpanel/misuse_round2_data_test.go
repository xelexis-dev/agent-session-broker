package controlpanel

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Match serverruntime's report deletion boundary without running a live broker.
type round2Backend struct {
	*misuseBackend
	store *Store
}

func (b *round2Backend) DeleteProject(ctx context.Context, name string) error {
	if err := b.reports.DeleteProject(name, b.store.ProjectGeneration(name)); err != nil {
		return err
	}
	return b.misuseBackend.DeleteProject(ctx, name)
}
func round2Fixture(t *testing.T) (*Store, *round2Backend, http.Handler) {
	t.Helper()
	s, b, _ := misuseFixture(t)
	backend := &round2Backend{b, s}
	return s, backend, New(s, backend, http.NotFoundHandler())
}
func seedRound2Audit(t *testing.T, s *Store, project, name string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e := AuditEvent{Version: 1, Project: project, OwnerID: "owner", Kind: "detached", At: now, Management: &protocol.ManagementEvent{Kind: "detached", ActorName: "panel", TargetName: name, TargetRegistrationID: "synthetic-audit-registration", At: now}}
	if err := s.commitAudited(s.data, []AuditEvent{e}); err != nil {
		t.Fatal(err)
	}
}
func TestRound2F5RecreatedProjectCannotReadDeletedAudit(t *testing.T) {
	s, _, h := round2Fixture(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	seedRound2Audit(t, s, "demo", "previous-owner-private-session")
	owner := loginPanelUser(t, h, "owner")
	if w := owner.call("GET", "/projects/audit?project=demo", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "previous-owner-private-session") {
		t.Fatal("audit fixture missing")
	}
	if w := owner.call("POST", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {"demo"}}); w.Code != 303 {
		t.Fatalf("delete=%d", w.Code)
	}
	if w := owner.call("GET", "/projects/audit?project=demo", nil); w.Code != 403 {
		t.Fatalf("deleted audit=%d", w.Code)
	}
	if err := s.CreateProjectFor("outsider", "demo"); err != nil {
		t.Fatal(err)
	}
	outsider := loginPanelUser(t, h, "outsider")
	w := outsider.call("GET", "/projects/audit?project=demo", nil)
	if w.Code != 200 {
		t.Fatalf("new project audit=%d", w.Code)
	}
	if strings.Contains(w.Body.String(), "previous-owner-private-session") {
		t.Fatal("new project owner can read deleted project's audit")
	}
	events, err := s.DirectAuditFor("outsider", "demo")
	if err != nil || len(events) != 0 {
		t.Fatal("deleted audit leaked through store")
	}
}

func TestRound2F5AuditDeletionAtomicAndPersistent(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"target", "unrelated"} {
		if err = s.CreateProjectFor("admin", name); err != nil {
			t.Fatal(err)
		}
		seedRound2Audit(t, s, name, name+"-session")
	}
	if _, err = s.beginProjectDeletion("admin", "target", "target"); err != nil {
		t.Fatal(err)
	}
	write := s.writeBatch
	s.writeBatch = func(map[string][]byte, []string) error { return errors.New("synthetic write failure") }
	if err = s.finishProjectDeletion("target"); err == nil {
		t.Fatal("write failure was ignored")
	}
	if _, ok := s.data.Projects["target"]; !ok || len(s.audits) != 2 {
		t.Fatal("failed transaction published partial state")
	}
	s.writeBatch = write
	if err = s.finishProjectDeletion("target"); err != nil {
		t.Fatal(err)
	}
	if len(s.audits) != 1 {
		t.Fatal("target audit not removed or unrelated audit lost")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.audits) != 1 {
		t.Fatal("audit removal did not persist")
	}
	for _, event := range reopened.audits {
		if event.Project != "unrelated" {
			t.Fatal("deleted audit returned after restart")
		}
	}
	if err = reopened.CreateProjectFor("admin", "target"); err != nil {
		t.Fatal(err)
	}
	for _, event := range reopened.audits {
		if event.Project == "target" {
			t.Fatal("deleted audit returned after recreation")
		}
	}
}
func TestRound2F5LegacyOrphanAuditDiscardedOnRecreation(t *testing.T) {
	s := userStore(t)
	seedRound2Audit(t, s, "demo", "orphan-private-session")
	// Reproduce the previous version's persisted deletion, which kept audit rows.
	s.mu.Lock()
	candidate := s.data
	candidate.Projects = maps.Clone(s.data.Projects)
	delete(candidate.Projects, "demo")
	raw, err := json.Marshal(candidate)
	if err == nil {
		err = s.write(raw)
	}
	if err == nil {
		s.data = candidate
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProjectFor("outsider", "demo"); err != nil {
		t.Fatal(err)
	}
	if len(s.audits) != 0 {
		t.Fatal("legacy orphan audit inherited by new project")
	}
}
func TestRound2F5ReportsDeletionAndConcurrentStatus(t *testing.T) {
	s, b, h := round2Fixture(t)
	if err := s.CreateUser("admin", "admin2", testPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	fixture := bugReportFixture()
	fixture.Reporter.ProjectGenerationID = s.ProjectGeneration("demo")
	report, err := b.reports.Create(fixture.Input, fixture.Reporter, "test")
	if err != nil {
		t.Fatal(err)
	}
	fixture.Reporter.Project = "foreign"
	fixture.Reporter.ProjectGenerationID = s.ProjectGeneration("foreign")
	fixture.Reporter.RegistrationID = "foreign-fixture"
	other, err := b.reports.Create(fixture.Input, fixture.Reporter, "test")
	if err != nil {
		t.Fatal(err)
	}
	a := loginPanelUser(t, h, "admin")
	c := loginPanelUser(t, h, "admin2")
	for range 8 {
		snapshot, err := b.reports.Get(report.ID)
		if err != nil {
			t.Fatal(err)
		}
		codes := concurrentRound2(t,
			func() int {
				return a.call("POST", "/admin/bug-reports/status", url.Values{"id": {report.ID}, "status": {"resolved"}, "expectedVersion": {snapshot.ExpectedVersion}}).Code
			},
			func() int {
				return c.call("POST", "/admin/bug-reports/status", url.Values{"id": {report.ID}, "status": {"open"}, "expectedVersion": {snapshot.ExpectedVersion}}).Code
			})
		for _, code := range codes {
			if code != 303 && code != 409 {
				t.Fatalf("status change=%d", code)
			}
		}
		got, err := b.reports.Get(report.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.StatusChangedAt.IsZero() || (got.Status == "closed" && got.StatusChangedBy != "admin") || (got.Status == "open" && got.StatusChangedBy != "admin2") {
			t.Fatal("status/actor/time torn by concurrency")
		}
	}
	report, err = b.reports.Get(report.ID)
	if err != nil {
		t.Fatal(err)
	}
	codes := concurrentRound2(t,
		func() int {
			return a.call("POST", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {"demo"}}).Code
		},
		func() int {
			return c.call("POST", "/admin/bug-reports/status", url.Values{"id": {report.ID}, "status": {"resolved"}, "expectedVersion": {report.ExpectedVersion}}).Code
		})
	for _, code := range codes {
		if code != 303 && code != 404 {
			t.Fatalf("delete/status=%d", code)
		}
	}
	if _, err = b.reports.Get(report.ID); !errors.Is(err, bugreport.ErrNotFound) {
		t.Fatal("report survived project deletion")
	}
	if _, err = b.reports.Get(other.ID); err != nil {
		t.Fatal("unrelated report removed")
	}
	for _, path := range []string{"/admin/bug-reports/detail?id=" + url.QueryEscape(report.ID), "/admin/bug-reports/status"} {
		method := "GET"
		if strings.HasSuffix(path, "status") {
			method = "POST"
		}
		w := a.call(method, path, url.Values{"id": {report.ID}, "status": {"open"}, "expectedVersion": {report.ExpectedVersion}})
		if w.Code != 404 {
			t.Fatalf("deleted report route=%d", w.Code)
		}
	}
}
