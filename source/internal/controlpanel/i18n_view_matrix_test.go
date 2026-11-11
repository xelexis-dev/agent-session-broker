package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

type panelLanguageView struct{ view, path string }

var panelLanguageViews = []panelLanguageView{
	{"setup", "/setup"}, {"login", "/login"}, {"projects", "/"},
	{"sessions", "/projects/sessions?project=demo"}, {"settings", "/projects/settings?project=demo"},
	{"audit", "/projects/audit?project=demo"}, {"profile", "/profile"},
	{"api-tokens", "/profile/api-tokens"}, {"api-token-new", "/profile/api-tokens/new"},
	{"authcode", "/auth/code"}, {"users", "/admin/users"}, {"user-detail", "/admin/users/detail?id=owner"},
	{"admin-settings", "/admin/settings"}, {"bug-reports", "/admin/bug-reports"},
	{"bug-report-detail", "/admin/bug-reports/detail?id=bug-demo-004"},
}

func matrixMissingViews(source string, views []panelLanguageView) ([]string, error) {
	covered := map[string]bool{}
	for _, row := range views {
		covered[row.view] = true
	}
	set, err := gateTemplates("views", []byte(source))
	if err != nil {
		return nil, err
	}
	dispatches := map[string]bool{}
	for _, tpl := range set.Templates() {
		var refs []string
		walkTemplate(tpl.Tree.Root, &refs, map[string]bool{}, dispatches)
	}
	var result []string
	for view := range dispatches {
		if !covered[view] {
			result = append(result, view)
		}
	}
	sort.Strings(result)
	return result, nil
}

func TestPanelViewMatrixCoversEveryDispatchBranch(t *testing.T) {
	raw, err := assets.ReadFile("templates.html")
	if err != nil {
		t.Fatal(err)
	}
	if missing, err := matrixMissingViews(string(raw), panelLanguageViews); err != nil || len(missing) != 0 {
		t.Errorf("views missing from both-language matrix: %v", missing)
	}
	injected := string(raw) + `{{if eq "future-view" $.View "another-view"}}{{end}}`
	if missing, err := matrixMissingViews(injected, panelLanguageViews); err != nil || !reflect.DeepEqual(missing, []string{"another-view", "future-view"}) {
		t.Errorf("new view escaped matrix inventory: %v", missing)
	}
}

func checkLanguagePage(t *testing.T, language, path, body string) {
	t.Helper()
	if !strings.Contains(body, `<html lang="`+language+`">`) {
		t.Errorf("%s %s: incorrect html language", language, path)
	}
	block := dataBlockRE.FindStringSubmatch(body)
	var actual, expected map[string]any
	canonical, err := json.Marshal(i18n.JS(language))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(canonical, &expected); err != nil {
		t.Fatal(err)
	}
	if block == nil || json.Unmarshal([]byte(block[1]), &actual) != nil || !reflect.DeepEqual(actual, expected) {
		t.Errorf("%s %s: wrong or missing JS catalog data block", language, path)
	}
	outside := dataBlockRE.ReplaceAllString(body, "")
	if placeholderRE.MatchString(outside) {
		t.Errorf("%s %s: untranslated placeholder", language, path)
	}
	if language == i18n.English && englishPageHasHangul(path, outside) {
		t.Errorf("English %s contains Hangul outside the reviewed native language option", path)
	}
}

func runPanelViewLanguageMatrix(t *testing.T) {
	store := userStore(t)
	if err := store.CreateUser("admin", "korean-admin", testPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	for user, language := range map[string]string{"admin": i18n.English, "korean-admin": i18n.Korean} {
		if err := store.UpdateLanguage(user, language); err != nil {
			t.Fatal(err)
		}
	}
	report := bugReportFixture()
	report.Reporter.ProjectGenerationID = store.ProjectGeneration("demo")
	// ASCII user-authored content keeps the gate about untranslated UI, without banning Korean user data.
	report.Input = bugreport.Input{Title: "Matrix report", Summary: "Matrix summary", Steps: "Matrix steps", Expected: "Matrix expected", Actual: "Matrix actual", Tool: "auth_status", Severity: "high"}
	reports := &fakeBugReports{reports: []bugreport.Report{report}}
	panel := &panel{store: store, backend: &panelBackend{}, next: http.NotFoundHandler(), sessions: map[string]loginSession{}, bugReports: reports, version: "matrix"}
	handler := http.NewCrossOriginProtection().Handler(http.HandlerFunc(panel.serve))
	clients := map[string]*userPanelClient{i18n.English: loginPanelUser(t, handler, "admin"), i18n.Korean: loginPanelUser(t, handler, "korean-admin")}
	for _, language := range i18n.Languages {
		for _, row := range panelLanguageViews {
			t.Run(language+"/"+row.view, func(t *testing.T) {
				t.Parallel()
				var response *httptest.ResponseRecorder
				if row.view == "setup" || row.view == "login" {
					target := handler
					if row.view == "setup" {
						empty, err := OpenSQLite(t.TempDir())
						if err != nil {
							t.Fatal(err)
						}
						defer empty.Close()
						target = New(empty, &panelBackend{}, http.NotFoundHandler())
					}
					request := httptest.NewRequest(http.MethodGet, row.path, nil)
					request.AddCookie(&http.Cookie{Name: languageCookie, Value: language})
					response = httptest.NewRecorder()
					target.ServeHTTP(response, request)
				} else {
					request := httptest.NewRequest(http.MethodGet, row.path, nil)
					request.AddCookie(clients[language].cookie)
					response = httptest.NewRecorder()
					handler.ServeHTTP(response, request)
				}
				if response.Code != http.StatusOK {
					t.Fatalf("%s %s: status%d", language, row.path, response.Code)
				}
				checkLanguagePage(t, language, row.path, response.Body.String())
			})
		}
	}
}
