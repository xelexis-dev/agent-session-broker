package controlpanel

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

type tooltipBackend struct{ panelBackend }

func (tooltipBackend) Sessions(context.Context, string) ([]Session, error) {
	return []Session{{ID: "tooltip-session", Name: "notice-session", Status: "missing", Harness: "codex", ProbePending: true, ErrorCode: "ingress-failed"}}, nil
}
func TestSessionStatusDescriptionsAreTooltips(t *testing.T) {
	h := New(userStore(t), &tooltipBackend{}, http.NotFoundHandler())
	body := loginPanelUser(t, h, "owner").call("GET", "/projects/sessions?project=demo", nil).Body.String()
	table := layoutTables(t, body)["프로젝트 연결 세션"]
	if strings.Join(table.headers, "|") != "계정|세션|하네스|상태|관리" {
		t.Errorf("project column remains: %v", table.headers)
	}
	trigger := regexp.MustCompile(`(?s)<button[^>]*aria-describedby="([^"]+)"[^>]*>.*?</button>`).FindStringSubmatch(body)
	if len(trigger) != 2 {
		t.Fatal("status badge has no described tooltip")
	}
	if strings.Contains(trigger[0], "title=") {
		t.Error("browser title tooltip used")
	}
	popup := regexp.MustCompile(`(?s)<span[^>]*id="` + regexp.QuoteMeta(trigger[1]) + `"[^>]*role="tooltip"[^>]*>(.*?)</span>`).FindStringSubmatch(body)
	if len(popup) != 2 {
		t.Fatal("badge description not in tooltip")
	}
	for _, text := range []string{"missing", "수신 연결 실패", "상태 응답 대기"} {
		if !strings.Contains(popup[1], text) {
			t.Errorf("tooltip lacks %s", text)
		}
	}
	cell := regexp.MustCompile(`(?s)<td[^>]*class="cell-status"[^>]*>(.*?)</td>`).FindStringSubmatch(body)
	if len(cell) != 2 {
		t.Fatal("status cell missing")
	}
	outside := strings.Replace(cell[1], popup[0], "", 1)
	for _, text := range []string{"missing", "수신 연결 실패", "상태 응답 대기"} {
		if strings.Contains(outside, text) {
			t.Errorf("description still outside tooltip: %s", text)
		}
	}
	if strings.Contains(body, `class="col-project"`) {
		t.Error("project cells remain")
	}
}
