package controlpanel

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func uiCopyPreview(view string) page {
	return page{View: view, Version: "dev", Project: "demo", Role: "owner", User: User{Username: "owner", ID: "owner", Role: "user", Enabled: true}, Selected: Project{Name: "demo", Mode: "token", Enabled: true}, SelectedUser: User{Username: "owner", ID: "owner", Role: "user", Enabled: true}, Form: url.Values{}, CanEdit: true, CanIssue: true, CanManage: map[string]bool{}, ScopeIssuanceReady: true, ScopeChoices: projectScopeChoices(url.Values{}), AutoCapValue: "8", AutoDaysValue: "30", ProjectTokens: []tokenView{{SessionToken: SessionToken{Kind: tokenProject, Label: "synthetic-auto", AdmissionMode: "auto", Scopes: []string{access.SessionRegister, access.SessionRegisterAuto}, MaxAutoBindings: 8}}}, AnnouncementComplete: true, AnnouncementAccepted: 1, AnnouncementFailed: 1}
}

func TestUICopyTrimRemovedLongStringsAndDescriptions(t *testing.T) {
	for _, view := range []string{"login", "setup", "projects", "settings", "audit", "sessions", "admin-settings", "users", "user-detail", "profile", "authcode"} {
		t.Run(view, func(t *testing.T) {
			v := uiCopyPreview(view)
			if view == "admin-settings" {
				v.User.Role = "admin"
			}
			if view == "settings" {
				v.IssuedToken = "SYNTHETIC-PREVIEW-NOT-A-CREDENTIAL"
			}
			var buf bytes.Buffer
			if err := pages.ExecuteTemplate(&buf, "page", v); err != nil {
				t.Fatal(err)
			}
			body := buf.String()
			for _, old := range []string{"사용량은 자동 등록 지원 후 제공", "접수는 모델 수신 확인이 아님", "LEGACY_V1"} {
				if strings.Contains(body, old) {
					t.Errorf("removed long string rendered: %s", old)
				}
			}
			for _, id := range []string{"project-token-help", "add-project-member-help", "profile-email-help", "create-project-help"} {
				if strings.Contains(body, `aria-describedby="`+id+`"`) {
					t.Errorf("dangling removed hint: %s", id)
				}
			}
			if view != "login" && view != "setup" {
				heading := regexp.MustCompile(`(?s)<header class="page-heading">.*?</header>`).FindString(body)
				if !strings.Contains(heading, "<h1>") {
					t.Error("page title removed")
				}
				if view == "admin-settings" {
					if !strings.Contains(heading, "<p>모든 프로젝트에 적용됩니다.</p>") {
						t.Error("admin description missing")
					}
				} else if strings.Contains(heading, "<p>") {
					t.Error("redundant header description remains")
				}
			}
		})
	}
}
