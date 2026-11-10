package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"net/url"
	"strings"
	"testing"
)

func TestBugReportPanelPostPreservesResolutionAndCAS(t *testing.T) {
	s, b, h := misuseFixture(t)
	fixture := bugReportFixture()
	fixture.Reporter.ProjectGenerationID = s.ProjectGeneration("demo")
	saved, err := b.reports.Create(fixture.Input, fixture.Reporter, "test")
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err = b.reports.SetResolution(saved.ID, "resolved", "fixed", bugreport.Actor{UserID: "admin", Source: "panel"}, saved.ExpectedVersion)
	if err != nil {
		t.Fatal(err)
	}
	c := loginPanelUser(t, h, "admin")
	body := c.call("GET", "/admin/bug-reports/detail?id="+saved.ID, nil).Body.String()
	if !strings.Contains(body, `name="expectedVersion" value="`+saved.ExpectedVersion+`"`) {
		t.Fatal("hidden version missing")
	}
	// Same-state requests preserve history; a stale subsequent transition conflicts.
	form := url.Values{"id": {saved.ID}, "status": {"resolved"}, "expectedVersion": {saved.ExpectedVersion}, "note": {"fixed"}}
	w := c.call("POST", "/admin/bug-reports/status", form)
	if w.Code != 303 {
		t.Fatal("same-state resolution", w.Code, w.Body.String())
	}
	got, err := b.reports.Get(saved.ID)
	if err != nil || got.Resolution != "resolved" || len(got.StatusChanges) != 1 || got.Note != "fixed" {
		t.Fatal("close lost precise state/history", err)
	}
	form.Set("status", "open")
	if w = c.call("POST", "/admin/bug-reports/status", form); w.Code != 303 {
		t.Fatal("reopen", w.Code)
	}
	got, _ = b.reports.Get(saved.ID)
	if got.Status != "open" || len(got.StatusChanges) != 2 || got.StatusChanges[0].To != "resolved" || got.Note != "fixed" {
		t.Fatal("reopen lost history")
	}
	form.Set("status", "resolved")
	if w = c.call("POST", "/admin/bug-reports/status", form); w.Code != 409 {
		t.Fatal("stale version accepted", w.Code)
	}
	form.Del("expectedVersion")
	if w = c.call("POST", "/admin/bug-reports/status", form); w.Code != 400 {
		t.Fatal("missing version accepted", w.Code)
	}
	// The precise panel filter and label expose the ignored classification.
	got, _, err = b.reports.SetResolution(got.ID, "ignored", "fixed", bugreport.Actor{UserID: "admin", Source: "panel"}, got.ExpectedVersion)
	if err != nil {
		t.Fatal(err)
	}
	body = c.call("GET", "/admin/bug-reports?status=ignored", nil).Body.String()
	if !strings.Contains(body, got.ID) || reportRow(got).StatusLabel != "무시" {
		t.Fatal("legacy list/filter/label")
	}
}
