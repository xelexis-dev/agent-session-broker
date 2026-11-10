package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSessionsSeparateRegistrationHistoryAndCurrentState(t *testing.T) {
	for _, tc := range []struct {
		code, reason string
		detach       bool
	}{
		{"detached", "연결 해제됨", false},
		{"server-restarted", "서버 재시작", true},
		{"ingress-failed", "수신 연결 실패", true},
		{"ingress-expired", "수신 연결 만료", true},
		{"probe-unanswered", "응답 없음 · 등록은 유지됩니다", true},
		{"identity-mismatch", "세션 식별 확인 실패 · 등록은 유지됩니다", true},
		{"unknown-reason", "unknown-reason", true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var session Session
			// Exercise the presentation boundary even before its reason field exists.
			if err := json.Unmarshal([]byte(`{"ID":"registration","Name":"example","Status":"missing","ErrorCode":"`+tc.code+`"}`), &session); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			render(w, 200, page{View: "sessions", Selected: Project{Mode: "token"}, User: User{Username: "owner"}, CanManage: map[string]bool{"registration": true}, Sessions: []Session{session}})
			body := w.Body.String()
			if !strings.Contains(body, tc.reason) {
				t.Errorf("missing state explanation %q", tc.reason)
			}
			if strings.Contains(body, `action="/sessions/detach"`) != tc.detach {
				t.Error("detach action does not match current state")
			}
			w = httptest.NewRecorder()
			render(w, 200, page{View: "settings", Selected: Project{Mode: "token"}, User: User{Username: "owner"}, CanIssue: true, CanEdit: true, Form: url.Values{}, ProjectTokens: []tokenView{{SessionToken: SessionToken{Kind: tokenProject, Label: "example", Approvals: 1, LastRegisteredAt: time.Date(2026, 9, 13, 1, 2, 0, 0, time.UTC)}, State: "registered"}}})
			body = w.Body.String()
			if !strings.Contains(body, "등록 이력") || !strings.Contains(body, "최근 등록") || !strings.Contains(body, ">등록 완료<") || strings.Contains(body, ">사용 중<") || strings.Contains(body, ">연결됨<") {
				t.Error("token registration history implies a current connection")
			}
		})
	}
}

func TestUnchangedMemberRoleHasAccurateFeedback(t *testing.T) {
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "owner")
	saved := c.call("POST", "/projects/members", url.Values{"project": {"demo"}, "username": {"developer"}, "action": {"update"}, "role": {"developer"}})
	if saved.Code != http.StatusSeeOther {
		t.Fatalf("save: %d", saved.Code)
	}
	req := httptest.NewRequest("GET", "http://broker"+saved.Header().Get("Location"), nil)
	req.AddCookie(c.cookie)
	for _, cookie := range saved.Result().Cookies() {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "변경된 멤버 정보가 없습니다.") || strings.Contains(w.Body.String(), "프로젝트 멤버 정보를 변경했습니다.") {
		t.Error("unchanged role reports a mutation")
	}
}

func TestDangerousRowActionsIdentifyTargetsAndUseSharedConfirmation(t *testing.T) {
	s := userStore(t)
	if _, err := s.IssueProjectTokenFor("owner", "demo", "example"); err != nil {
		t.Fatal(err)
	}
	c := loginPanelUser(t, New(s, &panelBackend{}, http.NotFoundHandler()), "owner")
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/projects/settings?project=demo", []string{`data-confirm-action="member-role"`, `data-confirm-action="member-remove"`, `data-confirm-target="developer"`, `aria-label="developer 프로젝트 역할 적용"`}},
		{"/projects/settings?project=demo", []string{`data-confirm-action="project-token-revoke"`, `data-confirm-target="example"`, `aria-label="example 프로젝트 토큰 취소"`}},
	} {
		body := c.call("GET", tc.path, nil).Body.String()
		for _, want := range append(tc.want, `id="confirm-action"`, `aria-describedby="confirm-action-description"`) {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing confirmation contract %s", tc.path, want)
			}
		}
	}
}
