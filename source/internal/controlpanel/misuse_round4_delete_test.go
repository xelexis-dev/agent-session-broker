package controlpanel

import (
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"net/url"
	"strings"
	"testing"
)

func TestMisuseK2MalformedDeletionIDsCannotAffectStoredReport(t *testing.T) {
	_, b, c, _, r := deletePanelFixture(t)
	for _, id := range []string{"../../bugreports.db", "bug-../x", "bug-한글", "bug-\x00x", strings.Repeat("z", 128<<10)} {
		f := deleteForm(r)
		f.Set("id", id)
		w := c.call("POST", "/admin/bug-reports/delete", f)
		if w.Code != 400 && w.Code != 404 && w.Code != 413 {
			t.Fatal("malformed ID response", w.Code)
		}
		if strings.Contains(w.Body.String(), "DELETE-SENTINEL") {
			t.Fatal("malformed ID leaked body")
		}
	}
	if _, err := b.reports.Get(r.ID); err != nil {
		t.Fatal("malformed ID modified real report", err)
	}
}

func TestMisuseK2DemotionAndAbsentDetails(t *testing.T) {
	s, b, c, _, r := deletePanelFixture(t)
	if err := s.CreateUser("admin", "deleteadmin", testPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	former := loginPanelUser(t, c.h, "deleteadmin")
	if err := s.UpdateUser("admin", "deleteadmin", "user", true, ""); err != nil {
		t.Fatal(err)
	}
	w := former.call("POST", "/admin/bug-reports/delete", deleteForm(r))
	if w.Code != 403 && w.Code != 303 {
		t.Fatal("demotion response", w.Code)
	}
	if _, err := b.reports.Get(r.ID); err != nil {
		t.Fatal("demoted admin deleted report", err)
	}
	if _, err := b.reports.DeletionFor("admin", r.ID); !errors.Is(err, bugreport.ErrNotFound) {
		t.Fatal("demotion journal side effect", err)
	}
	if w := c.call("POST", "/admin/bug-reports/delete", deleteForm(r)); w.Code != 303 {
		t.Fatal("admin delete", w.Code)
	}
	outsider := loginPanelUser(t, c.h, "outsider")
	for _, id := range []string{r.ID, "bug-nonexistent"} {
		w := outsider.call("GET", "/admin/bug-reports/detail?id="+url.QueryEscape(id), nil)
		if w.Code != 403 || strings.Contains(w.Body.String(), "DELETE-SENTINEL") || strings.Contains(w.Body.String(), "삭제했습니다") {
			t.Fatal("non-admin absent lookup disclosed data", w.Code)
		}
	}
}
