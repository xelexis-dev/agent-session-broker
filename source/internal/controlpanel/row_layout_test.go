package controlpanel

import (
	"context"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// Narrow layouts show each table cell as a labelled card line, so every cell
// must keep the context that its column header gives on wide screens.

type layoutBackend struct{}

func (layoutBackend) Sessions(context.Context, string) ([]Session, error) {
	return []Session{
		{ID: "reg-a", SessionID: "11111111-2222-3333-4444-555555555555", Name: "agent-a", Status: "alive", Harness: "codex", PID: 42, Ready: true, ClientID: "pc-client-a"},
		{ID: "reg-b", SessionID: "66666666-7777-8888-9999-000000000000", Name: "agent-b", Status: "missing", Harness: "claude", PID: 43, ErrorCode: "detached"},
	}, nil
}
func (layoutBackend) Detach(context.Context, string, string) error { return nil }

var (
	layoutTableRE   = regexp.MustCompile(`(?s)<table\b[^>]*>(.*?)</table>`)
	layoutCaptionRE = regexp.MustCompile(`(?s)<caption\b[^>]*>(.*?)</caption>`)
	layoutHeadRE    = regexp.MustCompile(`(?s)<thead\b[^>]*>(.*?)</thead>`)
	layoutBodyRE    = regexp.MustCompile(`(?s)<tbody\b[^>]*>(.*?)</tbody>`)
	layoutRowRE     = regexp.MustCompile(`(?s)<tr\b[^>]*>(.*?)</tr>`)
	layoutHeaderRE  = regexp.MustCompile(`(?s)<th\b[^>]*>(.*?)</th>`)
	layoutCellRE    = regexp.MustCompile(`(?s)<td\b([^>]*)>(.*?)</td>`)
	layoutTagRE     = regexp.MustCompile(`<[^>]+>`)
)

type layoutCell struct{ attrs, text string }

type layoutTable struct {
	headers []string
	rows    [][]layoutCell
}

func layoutText(fragment string) string {
	return strings.Join(strings.Fields(html.UnescapeString(layoutTagRE.ReplaceAllString(fragment, " "))), " ")
}

func layoutAttr(attrs, name string) (string, bool) {
	m := regexp.MustCompile(`\b` + name + `="([^"]*)"`).FindStringSubmatch(attrs)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(m[1]), true
}

func layoutTables(t *testing.T, body string) map[string]layoutTable {
	t.Helper()
	tables := map[string]layoutTable{}
	for _, m := range layoutTableRE.FindAllStringSubmatch(body, -1) {
		caption := layoutCaptionRE.FindStringSubmatch(m[1])
		head := layoutHeadRE.FindStringSubmatch(m[1])
		tbody := layoutBodyRE.FindStringSubmatch(m[1])
		if caption == nil || head == nil || tbody == nil {
			t.Fatalf("table without caption, thead or tbody: %s", m[0])
		}
		var table layoutTable
		for _, th := range layoutHeaderRE.FindAllStringSubmatch(head[1], -1) {
			table.headers = append(table.headers, layoutText(th[1]))
		}
		for _, tr := range layoutRowRE.FindAllStringSubmatch(tbody[1], -1) {
			var row []layoutCell
			for _, td := range layoutCellRE.FindAllStringSubmatch(tr[1], -1) {
				row = append(row, layoutCell{attrs: td[1], text: layoutText(td[2])})
			}
			table.rows = append(table.rows, row)
		}
		tables[layoutText(caption[1])] = table
	}
	return tables
}

func TestEmptyTableStateSpansRenderedColumns(t *testing.T) {
	s := userStore(t)
	h := New(s, layoutBackend{}, http.NotFoundHandler())
	for _, tc := range []struct {
		user        string
		wantColumns int
		wantColspan string
	}{
		{user: "owner", wantColumns: 7, wantColspan: "7"},
		{user: "viewer", wantColumns: 6, wantColspan: "6"},
	} {
		t.Run(tc.user, func(t *testing.T) {
			page := loginPanelUser(t, h, tc.user).call("GET", "/projects/settings?project=demo", nil)
			tokens, ok := layoutTables(t, page.Body.String())[en("tokens.project_tokens")]
			if page.Code != 200 || !ok {
				t.Fatalf("token table missing: %d", page.Code)
			}
			if len(tokens.headers) != tc.wantColumns || len(tokens.rows) != 1 || len(tokens.rows[0]) != 1 {
				t.Fatalf("unexpected empty token table: headers=%q rows=%d", tokens.headers, len(tokens.rows))
			}
			if got, _ := layoutAttr(tokens.rows[0][0].attrs, "colspan"); got != tc.wantColspan {
				t.Fatalf("empty state colspan = %q, want %q", got, tc.wantColspan)
			}
		})
	}
}

func TestTableCellsCarryTheirColumnLabel(t *testing.T) {
	s := userStore(t)
	if _, err := s.IssueProjectTokenFor("developer", "demo", "agent-a"); err != nil {
		t.Fatal(err)
	}
	h := New(s, layoutBackend{}, http.NotFoundHandler())
	for _, tc := range []struct {
		user, path, caption string
		wantRows            int
	}{
		{user: "owner", path: "/projects/settings?project=demo", caption: en("tokens.project_tokens"), wantRows: 1},
		{user: "owner", path: "/projects/sessions?project=demo", caption: en("sessions.caption"), wantRows: 2},
		{user: "viewer", path: "/projects/sessions?project=demo", caption: en("sessions.caption"), wantRows: 2},
		{user: "owner", path: "/projects/settings?project=demo", caption: en("tokens.project_users_and_roles"), wantRows: 3},
		{user: "viewer", path: "/projects/settings?project=demo", caption: en("tokens.project_users_and_roles"), wantRows: 3},
	} {
		t.Run(tc.user+" "+tc.caption, func(t *testing.T) {
			page := loginPanelUser(t, h, tc.user).call("GET", tc.path, nil)
			table, ok := layoutTables(t, page.Body.String())[tc.caption]
			if page.Code != 200 || !ok || len(table.rows) != tc.wantRows {
				t.Fatalf("table %q missing or rows=%d", tc.caption, len(table.rows))
			}
			for _, row := range table.rows {
				if len(row) != len(table.headers) {
					t.Fatalf("row has %d cells for %d headers", len(row), len(table.headers))
				}
				for i, cell := range row {
					label, ok := layoutAttr(cell.attrs, "data-label")
					if !ok || label != table.headers[i] {
						t.Fatalf("cell %d (%q) label = %q, want column %q", i, cell.text, label, table.headers[i])
					}
				}
			}
		})
	}
}

type pidLayoutBackend struct {
	layoutBackend
	zero bool
}

func (b pidLayoutBackend) Sessions(ctx context.Context, project string) ([]Session, error) {
	rows, err := b.layoutBackend.Sessions(ctx, project)
	if b.zero {
		for i := range rows {
			rows[i].PID = 0
		}
	}
	return rows, err
}

func TestSessionRowsKeepOnlyInlinePID(t *testing.T) {
	for _, zero := range []bool{false, true} {
		h := New(userStore(t), pidLayoutBackend{zero: zero}, http.NotFoundHandler())
		page := loginPanelUser(t, h, "owner").call("GET", "/projects/sessions?project=demo", nil)
		cells := regexp.MustCompile(`(?s)<td[^>]*class="cell-primary"[^>]*>(.*?)</td>`).FindAllStringSubmatch(page.Body.String(), -1)
		if page.Code != 200 || len(cells) != 2 {
			t.Fatal("session primary cells missing")
		}
		for i, cell := range cells {
			for _, forbidden := range []string{"11111111-2222-3333-4444-555555555555", "66666666-7777-8888-9999-000000000000", "reg-a", "reg-b", "pc-client-a", "· PC"} {
				if strings.Contains(cell[1], forbidden) {
					t.Fatal("primary cell exposes removed identity metadata")
				}
			}
			if zero {
				if strings.Contains(cell[1], "session-pid") {
					t.Fatal("zero PID must not render")
				}
			} else {
				want := []string{`<strong>agent-a</strong><small class="mono cell-meta session-pid">PID 42</small>`, `<strong>agent-b</strong><small class="mono cell-meta session-pid">PID 43</small>`}[i]
				if !strings.Contains(cell[1], want) {
					t.Fatal("session name must be followed by inline PID")
				}
			}
			if !strings.Contains(cell[1], "data-session-dot") || !strings.Contains(cell[1], "data-runtime-info") {
				t.Fatal("state dot or runtime line missing")
			}
		}
	}
}

func TestUserDetailControlsNameTheirUser(t *testing.T) {
	h := New(userStore(t), layoutBackend{}, http.NotFoundHandler())
	body := loginPanelUser(t, h, "admin").call("GET", "/admin/users/detail?id=developer", nil).Body.String()
	form := regexp.MustCompile(`(?s)<form method="post" action="/admin/users/update"[^>]*><input type="hidden" name="csrf" value="[^"]*"><input type="hidden" name="id" value="developer">(.*?)</form>`).FindStringSubmatch(body)
	if form == nil {
		t.Fatal("developer edit form missing")
	}
	for _, tc := range []struct{ control, visible string }{
		{control: `<select name="role"`, visible: en("admin.users.system_role")},
		{control: `<input type="password" name="password"`, visible: en("profile.new_password")},
		{control: `<input type="checkbox" name="enabled"`, visible: en("admin.users.account_enabled")},
		{control: `<button`, visible: en("admin.users.save")},
	} {
		start := strings.Index(form[1], tc.control)
		if start < 0 {
			t.Fatalf("%s missing", tc.control)
		}
		tag := form[1][start : start+strings.Index(form[1][start:], ">")]
		label, ok := layoutAttr(tag, "aria-label")
		if !ok || !strings.Contains(label, "developer") || !strings.Contains(label, tc.visible) {
			t.Fatalf("%s accessible name = %q, want user and %q", tc.control, label, tc.visible)
		}
	}
}
