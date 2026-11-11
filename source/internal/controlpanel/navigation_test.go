package controlpanel

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var (
	navRE  = regexp.MustCompile(`(?s)<nav aria-label="` + en("nav.main") + `"[^>]*>(.*?)</nav>`)
	menuRE = regexp.MustCompile(`(?s)<div id="user-menu-items"(.*?)</aside>`)
)

func fragment(t *testing.T, re *regexp.Regexp, body string) string {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("fragment %s missing", re)
	}
	return m[1]
}

// The approved design groups the sidebar: 프로젝트 (list, and the current
// project's sessions/settings), 계정 (profile, session approval) and 관리자
// 전용 for system administrators only. The user menu keeps profile, admin
// panel and logout.
func TestSidebarGroupsFollowRoleAndProject(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	for _, tc := range []struct {
		user, path, current string
		admin, project      bool
	}{
		{"owner", "/", `href="/"`, false, false},
		{"owner", "/projects/sessions?project=demo", `href="/projects/sessions?project=demo"`, false, true},
		{"owner", "/projects/settings?project=demo", `href="/projects/settings?project=demo"`, false, true},
		{"owner", "/profile", `href="/profile"`, false, false},
		{"owner", "/auth/code", `href="/auth/code"`, false, false},
		{"admin", "/", `href="/"`, true, false},
	} {
		t.Run(tc.user+" "+tc.path, func(t *testing.T) {
			page := loginPanelUser(t, h, tc.user).call("GET", tc.path, nil)
			if page.Code != 200 {
				t.Fatalf("status %d", page.Code)
			}
			body := page.Body.String()
			nav := fragment(t, navRE, body)
			for _, want := range []string{`href="/"`, ">" + en("nav.projects") + "<", `href="/profile"`, `href="/auth/code"`, ">" + en("nav.group_projects") + "<", ">" + en("nav.group_account") + "<"} {
				if !strings.Contains(nav, want) {
					t.Errorf("nav lacks %s", want)
				}
			}
			if strings.Contains(nav, `href="/projects/sessions?project=demo"`) != tc.project || strings.Contains(nav, `href="/projects/settings?project=demo"`) != tc.project {
				t.Errorf("project items visibility = %v", !tc.project)
			}
			if strings.Contains(nav, `href="/admin/users"`) != tc.admin || strings.Contains(nav, `href="/admin/settings"`) != tc.admin || strings.Contains(nav, en("nav.group_admin")) != tc.admin {
				t.Errorf("admin group visibility = %v", !tc.admin)
			}
			if got := strings.Count(nav, `aria-current="page"`); got != 1 || !regexp.MustCompile(regexp.QuoteMeta(tc.current)+`[^>]*aria-current="page"`).MatchString(nav) {
				t.Errorf("current item is not %s (%d marked)", tc.current, got)
			}
			menu := fragment(t, menuRE, body)
			for _, want := range []string{`role="menu"`, `href="/profile"`, `action="/logout"`, `role="menuitem"`} {
				if !strings.Contains(menu, want) {
					t.Errorf("user menu lacks %s", want)
				}
			}
			if strings.Contains(menu, `href="/admin/users"`) != tc.admin {
				t.Errorf("admin panel link visibility = %v", !tc.admin)
			}
			if !strings.Contains(body, `data-user-menu-toggle`) || !strings.Contains(body, `id="user-menu-items" class="user-menu-items" role="menu" aria-label="`+en("shell.user_menu")+`" hidden`) {
				t.Error("user menu button or hidden menu missing")
			}
		})
	}
}

func TestAdminPagesMarkTheirSidebarItem(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	for _, path := range []string{"/admin/users", "/admin/settings"} {
		nav := fragment(t, navRE, admin.call("GET", path, nil).Body.String())
		if !regexp.MustCompile(`href="`+regexp.QuoteMeta(path)+`"[^>]*aria-current="page"`).MatchString(nav) || !strings.Contains(nav, `href="/"`) {
			t.Errorf("%s nav does not mark its item", path)
		}
	}
	owner := loginPanelUser(t, h, "owner")
	for _, path := range []string{"/admin/users", "/admin/settings"} {
		if w := owner.call("GET", path, nil); w.Code != 403 {
			t.Errorf("non-admin %s = %d", path, w.Code)
		}
	}
}

func TestProjectListIsSearchableTableWithRowLinks(t *testing.T) {
	s := userStore(t)
	if err := s.CreateProjectFor("owner", "second"); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	body := owner.call("GET", "/", nil).Body.String()
	table, ok := layoutTables(t, body)[en("projects.projects")]
	if !ok || len(table.rows) != 2 || strings.Join(table.headers, "|") != strings.Join([]string{en("projects.project"), en("projects.my_role"), en("projects.status"), en("projects.updated")}, "|") {
		t.Fatalf("project table = %+v", table)
	}
	if !strings.Contains(table.rows[0][0].text, "demo") || table.rows[0][1].text != "Owner" || !strings.Contains(table.rows[0][2].text, en("projects.enabled")) {
		t.Fatalf("first row = %+v", table.rows[0])
	}
	for _, want := range []string{`data-table-filter="project-table"`, `id="project-table"`, `data-row-links`, `class="row-link" data-filter-text="demo"`, `class="row-link-target" href="/projects/sessions?project=demo"`, `type="search"`} {
		if !strings.Contains(body, want) {
			t.Errorf("project list lacks %s", want)
		}
	}
	if strings.Contains(body, `href="/projects/settings?project=demo"`) {
		t.Error("project rows link to settings instead of detail")
	}
	if w := owner.call("GET", "/?project=demo", nil); w.Code != 303 || w.Header().Get("Location") != "/projects/sessions?project=demo" {
		t.Errorf("legacy detail redirect = %d %s", w.Code, w.Header().Get("Location"))
	}
	developer := loginPanelUser(t, h, "developer").call("GET", "/", nil).Body.String()
	if rows := layoutTables(t, developer)[en("projects.projects")].rows; len(rows) != 1 || rows[0][1].text != "Developer" {
		t.Errorf("developer rows = %+v", rows)
	}
}

func TestProjectDetailAndSettingsLinkEachOther(t *testing.T) {
	h := New(userStore(t), layoutBackend{}, http.NotFoundHandler())
	viewer := loginPanelUser(t, h, "viewer")
	detail := viewer.call("GET", "/projects/sessions?project=demo", nil).Body.String()
	for _, want := range []string{`aria-label="` + en("nav.project_settings") + `"`, `href="/projects/settings?project=demo"`, `data-table-filter="session-table"`, `id="session-table"`, `class="icon icon--settings"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %s", want)
		}
	}
	tables := layoutTables(t, detail)
	if _, ok := tables[en("sessions.caption")]; !ok {
		t.Error("detail lacks the session table")
	}
	if _, ok := tables[en("tokens.project_tokens")]; ok {
		t.Error("detail still lists tokens")
	}
	settings := viewer.call("GET", "/projects/settings?project=demo", nil).Body.String()
	if !strings.Contains(settings, `href="/projects/sessions?project=demo"`) || !strings.Contains(settings, en("heading.back_project")) {
		t.Error("settings lacks the back link to detail")
	}
	if !strings.Contains(settings, "<h1>"+en("heading.project_settings", "project", "demo")+"</h1>") || !strings.Contains(detail, "<h1>demo</h1>") {
		t.Error("page headings do not distinguish detail and settings")
	}
}

func TestProjectSettingsEntrancesUseSettingsIcon(t *testing.T) {
	h := New(userStore(t), layoutBackend{}, http.NotFoundHandler())
	viewer := loginPanelUser(t, h, "viewer")
	for _, path := range []string{"/projects/sessions?project=demo", "/projects/settings?project=demo"} {
		page := viewer.call("GET", path, nil)
		if page.Code != http.StatusOK {
			t.Fatalf("%s status %d", path, page.Code)
		}
		links := regexp.MustCompile(`<a[^>]*href="/projects/settings\?project=demo"[^>]*>(.*?)</a>`).FindAllStringSubmatch(page.Body.String(), -1)
		iconLinks := 0
		for _, link := range links {
			if !strings.Contains(link[1], `class="icon `) {
				continue
			}
			iconLinks++
			if !strings.Contains(link[1], `class="icon icon--settings"`) || strings.Contains(link[1], "icon--download") || strings.Contains(link[1], "icon--chart-no-axes-combined") {
				t.Errorf("%s project settings uses wrong icon: %s", path, link[1])
			}
		}
		want := 1
		if strings.Contains(path, "/sessions?") {
			want = 2
		}
		if iconLinks != want {
			t.Errorf("%s icon entrances = %d, want %d", path, iconLinks, want)
		}
	}
}
