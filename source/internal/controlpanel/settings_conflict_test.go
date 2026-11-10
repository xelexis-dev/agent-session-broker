package controlpanel

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func settingsFormVersion(body, action string) string {
	form := regexp.MustCompile(`(?s)<form[^>]+action="` + regexp.QuoteMeta(action) + `"[^>]*>(.*?)</form>`).FindStringSubmatch(body)
	if len(form) != 2 {
		return ""
	}
	value := regexp.MustCompile(`name="settings_version" value="([^"]*)"`).FindStringSubmatch(form[1])
	if len(value) != 2 {
		return ""
	}
	return value[1]
}
func TestSettingsConflictProjectStaleForm(t *testing.T) {
	t.Parallel()
	s := userStore(t)
	if err := s.CreateUser("admin", "admin2", testPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	a, b := loginPanelUser(t, h, "admin"), loginPanelUser(t, h, "admin2")
	av := settingsFormVersion(a.call("GET", "/projects/settings?project=demo", nil).Body.String(), "/projects/save")
	bv := settingsFormVersion(b.call("GET", "/projects/settings?project=demo", nil).Body.String(), "/projects/save")
	if w := a.call("POST", "/projects/save", url.Values{"name": {"demo"}, "settings_version": {av}}); w.Code != 303 {
		t.Fatalf("first save=%d", w.Code)
	}
	w := b.call("POST", "/projects/save", url.Values{"name": {"demo"}, "enabled": {"true"}, "settings_version": {bv}})
	if w.Code != 409 {
		t.Fatalf("stale save=%d; must reject overwriting first admin", w.Code)
	}
	if s.ProjectsFor("admin")[0].Enabled {
		t.Fatal("stale form overwrote first admin")
	}
	fresh := settingsFormVersion(w.Body.String(), "/projects/save")
	if fresh == "" || fresh == bv {
		t.Fatal("conflict lacks fresh version")
	}
}

type settingsConflictCase struct {
	name, page, action string
	first, stale       url.Values
}

func settingsConflictCases() []settingsConflictCase {
	return []settingsConflictCase{
		{"project", "/projects/settings?project=demo", "/projects/save", url.Values{"name": {"demo"}}, url.Values{"name": {"demo"}, "enabled": {"true"}}},
		{"http", "/admin/settings", "/admin/http-policy", url.Values{"allowHTTP": {"true"}}, url.Values{"allowHTTP": {"false"}}},
		{"scope", "/admin/settings", "/admin/scope-issuance", url.Values{"enabled": {"true"}}, url.Values{"enabled": {"false"}}},
		{"user", "/admin/users/detail?id=viewer", "/admin/users/update", url.Values{"id": {"viewer"}, "role": {"user"}}, url.Values{"id": {"viewer"}, "role": {"user"}, "enabled": {"true"}}},
		{"profile", "/profile", "/profile/save", url.Values{"displayName": {"Current name"}, "email": {"current@example.test"}}, url.Values{"displayName": {"Stale name"}, "email": {"stale@example.test"}}},
	}
}
func conflictForm(t *testing.T, body, action string) string {
	t.Helper()
	form := regexp.MustCompile(`(?s)<form[^>]+action="` + regexp.QuoteMeta(action) + `"[^>]*>(.*?)</form>`).FindStringSubmatch(body)
	if len(form) != 2 {
		t.Fatal("settings form missing")
	}
	return form[1]
}
func TestSettingsConflictForms(t *testing.T) {
	t.Parallel()
	for _, tc := range settingsConflictCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := userStore(t)
			if err := s.CreateUser("admin", "admin2", testPassword, "admin"); err != nil {
				t.Fatal(err)
			}
			h := New(s, &panelBackend{}, http.NotFoundHandler())
			a := loginPanelUser(t, h, "admin")
			actorB := "admin2"
			if tc.name == "profile" {
				actorB = "admin"
			}
			b := loginPanelUser(t, h, actorB)
			old := settingsFormVersion(b.call("GET", tc.page, nil).Body.String(), tc.action)
			if len(old) != 64 {
				t.Fatal("missing rendered hash")
			}
			av := settingsFormVersion(a.call("GET", tc.page, nil).Body.String(), tc.action)
			tc.first.Set("settings_version", av)
			if w := a.call("POST", tc.action, tc.first); w.Code != 303 {
				t.Fatalf("first=%d", w.Code)
			}
			before := misuseSnapshot(t, s)
			tc.stale.Set("settings_version", old)
			w := b.call("POST", tc.action, tc.stale)
			if w.Code != 409 || !strings.Contains(w.Body.String(), settingsConflictMessage) {
				t.Fatalf("conflict=%d", w.Code)
			}
			if misuseSnapshot(t, s) != before {
				t.Fatal("conflict changed persisted state")
			}
			fresh := settingsFormVersion(w.Body.String(), tc.action)
			currentPage := b.call("GET", tc.page, nil).Body.String()
			if fresh == old || len(fresh) != 64 || fresh != settingsFormVersion(currentPage, tc.action) {
				t.Fatal("version not refreshed")
			}
			// All editable fields in the conflict form must be the same as a fresh GET.
			// CSRF and version are stable; these forms have no one-use submission fields.
			if conflictForm(t, w.Body.String(), tc.action) != conflictForm(t, currentPage, tc.action) {
				t.Fatal("conflict form is not current stored values")
			}
			for _, bad := range [][]string{{""}, {"garbled"}, {strings.Repeat("0", 64)}, {fresh, fresh}, {strings.ToUpper(fresh)}} {
				tc.stale["settings_version"] = bad
				if got := b.call("POST", tc.action, tc.stale); got.Code != 409 {
					t.Fatalf("invalid version=%d", got.Code)
				}
				if misuseSnapshot(t, s) != before {
					t.Fatal("invalid version changed state")
				}
			}
			tc.stale.Set("settings_version", fresh)
			if w := b.call("POST", tc.action, tc.stale); w.Code != 303 {
				t.Fatalf("fresh retry=%d", w.Code)
			}
			// Script/pre-upgrade form compatibility is intentional, including stale values.
			tc.first.Del("settings_version")
			if w := b.call("POST", tc.action, tc.first); w.Code != 303 {
				t.Fatalf("legacy save=%d", w.Code)
			}
		})
	}
}
func TestSettingsConflictConcurrentSaves(t *testing.T) {
	t.Parallel()
	for _, tc := range settingsConflictCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := userStore(t)
			h := New(s, &panelBackend{}, http.NotFoundHandler())
			a := loginPanelUser(t, h, "admin")
			b := loginPanelUser(t, h, "admin")
			version := settingsFormVersion(a.call("GET", tc.page, nil).Body.String(), tc.action)
			tc.first.Set("settings_version", version)
			codes := concurrentRound2(t, func() int { return a.call("POST", tc.action, tc.first).Code }, func() int { return b.call("POST", tc.action, tc.first).Code })
			winners, conflicts := 0, 0
			for _, code := range codes {
				if code == 303 {
					winners++
				} else if code == 409 {
					conflicts++
				} else {
					t.Fatalf("save=%d", code)
				}
			}
			if winners != 1 || conflicts != 1 {
				t.Fatal("compare and save were not atomic")
			}
		})
	}
}
func TestSettingsConflictAuthorizationAndNamespaces(t *testing.T) {
	t.Parallel()
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	viewer := loginPanelUser(t, h, "viewer")
	before := misuseSnapshot(t, s)
	for _, tc := range settingsConflictCases() {
		v := settingsFormVersion(admin.call("GET", tc.page, nil).Body.String(), tc.action)
		values := tc.first
		values.Set("settings_version", v)
		if w := misuseRequest(h, admin.cookie, "POST", tc.action, values); w.Code != 403 {
			t.Fatalf("missing CSRF on %s=%d", tc.name, w.Code)
		}
		if tc.name != "profile" {
			if w := viewer.call("POST", tc.action, values); w.Code != 403 {
				t.Fatalf("permission on %s=%d", tc.name, w.Code)
			}
		}
	}
	if misuseSnapshot(t, s) != before {
		t.Fatal("unauthorized request changed state")
	}
	if err := s.CreateProjectFor("admin", "other"); err != nil {
		t.Fatal(err)
	}
	v := settingsFormVersion(admin.call("GET", "/projects/settings?project=demo", nil).Body.String(), "/projects/save")
	if w := admin.call("POST", "/projects/save", url.Values{"name": {"other"}, "settings_version": {v}}); w.Code != 409 {
		t.Fatalf("different target=%d", w.Code)
	}
	policy := settingsFormVersion(admin.call("GET", "/admin/settings", nil).Body.String(), "/admin/http-policy")
	if w := admin.call("POST", "/admin/scope-issuance", url.Values{"enabled": {"true"}, "settings_version": {policy}}); w.Code != 409 {
		t.Fatalf("different form=%d", w.Code)
	}
	// Member updates don't invalidate the unrelated access checkbox snapshot.
	if err := s.SetProjectMember("admin", "demo", "viewer", "developer"); err != nil {
		t.Fatal(err)
	}
	if w := admin.call("POST", "/projects/save", url.Values{"name": {"demo"}, "settings_version": {v}}); w.Code != 303 {
		t.Fatalf("unrelated member change=%d", w.Code)
	}
}

func TestSettingsConflictValidationRetryKeepsOriginalBase(t *testing.T) {
	t.Parallel()
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	a := loginPanelUser(t, h, "admin")
	b := loginPanelUser(t, h, "admin")
	old := settingsFormVersion(b.call("GET", "/profile", nil).Body.String(), "/profile/save")
	if w := a.call("POST", "/profile/save", url.Values{"displayName": {"Newer"}, "email": {"newer@example.test"}, "settings_version": {old}}); w.Code != 303 {
		t.Fatal("first save failed")
	}
	// An invalid field must not turn a stale base into a fresh base silently.
	w := b.call("POST", "/profile/save", url.Values{"displayName": {"Old form"}, "email": {"invalid address"}, "settings_version": {old}})
	if w.Code != 409 {
		t.Fatalf("stale invalid form=%d, want conflict before value validation", w.Code)
	}
	fresh := settingsFormVersion(w.Body.String(), "/profile/save")
	bad := b.call("POST", "/profile/save", url.Values{"displayName": {"Typed"}, "email": {"invalid address"}, "settings_version": {fresh}})
	if bad.Code != 400 || settingsFormVersion(bad.Body.String(), "/profile/save") != fresh {
		t.Fatal("ordinary invalid form lost its base")
	}
}
