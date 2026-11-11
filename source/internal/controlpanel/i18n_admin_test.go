package controlpanel

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

// englishBugReport is the shared fixture with English report text, so Korean on the page can only come from the panel.
func englishBugReport() bugreport.Report {
	r := bugReportFixture()
	r.Input.Title, r.Input.Summary = "Ready state is stale", "A message arrived but the ready state did not change."
	r.Input.Steps, r.Input.Expected, r.Input.Actual = "1. Connect a session.\n2. Send a message.", "Ready to receive.", "Not ready."
	return r
}

var adminPages = []string{"/admin/users", "/admin/users/detail?id=owner", "/admin/settings", "/admin/bug-reports",
	"/admin/bug-reports/detail?id=bug-demo-004", "/auth/code"}

// MR-5: the admin pages and session approval follow the account language.
func TestAdminPagesFollowTheLanguage(t *testing.T) {
	s := userStore(t)
	p := &panel{store: s, backend: &panelBackend{}, next: http.NotFoundHandler(), sessions: map[string]loginSession{}, bugReports: &fakeBugReports{reports: []bugreport.Report{englishBugReport()}}, version: "0.17.1"}
	admin := loginPanelUser(t, http.NewCrossOriginProtection().Handler(http.HandlerFunc(p.serve)), "admin")
	for _, path := range adminPages {
		w := admin.call("GET", path, nil)
		outside := dataBlockRE.ReplaceAllString(w.Body.String(), "")
		if w.Code != http.StatusOK || !strings.Contains(outside, `<html lang="en">`) {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		if korean := hangulRE.FindAllStringIndex(outside, -1); len(korean) > 0 {
			at := korean[0][0]
			t.Errorf("%s: English page has Korean near %q", path, outside[max(0, at-40):min(len(outside), at+60)])
		}
	}
	if err := s.UpdateLanguage("admin", "ko"); err != nil {
		t.Fatal(err)
	}
	for _, path := range adminPages {
		w := admin.call("GET", path, nil)
		if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, `<html lang="ko">`) || !hangulRE.MatchString(dataBlockRE.ReplaceAllString(body, "")) {
			t.Errorf("%s: Korean page missing", path)
		}
	}
}

// Announcement failures arrive as machine codes from serverruntime; every code has a message and an unknown code
// reads as "not accepted" rather than leaking the raw value.
func TestAnnouncementErrorsAreCodes(t *testing.T) {
	for code, key := range announcementErrors {
		if got := (AnnouncementResult{Error: code}).ErrorMessage(); got.Key != key || !i18n.Has(key) {
			t.Errorf("%s -> %q", code, got.Key)
		}
	}
	if got := (AnnouncementResult{Error: "future-code"}).ErrorMessage().In(i18n.English); got != en("admin.announce.error_not_accepted") {
		t.Errorf("unknown code = %q", got)
	}
	src, err := os.ReadFile("../serverruntime/announcement.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range regexp.MustCompile(`Error: "([a-z-]+)"|result\.Error = "([a-z-]+)"`).FindAllStringSubmatch(string(src), -1) {
		if _, ok := announcementErrors[code[1]+code[2]]; !ok {
			t.Errorf("serverruntime emits unmapped code %q", code[1]+code[2])
		}
	}
}
