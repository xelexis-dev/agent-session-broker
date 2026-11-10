package controlpanel

import (
	"net/http"
	"strings"
	"testing"
)

func TestSessionStatsAreCompactInsideProjectManagement(t *testing.T) {
	f := newDeviceFixture(t)
	f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "reserved-stat")
	b := &reservedGrantBackend{sessions: []Session{{ID: "ready", Name: "ready", Ready: true}, {ID: "missing", Name: "missing", ErrorCode: "server-restarted"}, {ID: "suspended", Name: "suspended", Suspended: true}}}
	c := loginPanelUser(t, New(f.store, b, http.NotFoundHandler()), "owner")
	body := c.call("GET", "/projects/sessions?project=demo", nil).Body.String()
	start := strings.Index(body, `aria-labelledby="manage-title"`)
	if start < 0 {
		t.Fatal("project management card missing")
	}
	end := strings.Index(body[start:], "</section>")
	if end < 0 {
		t.Fatal("project management card not closed")
	}
	manage := body[start : start+end]
	for _, stat := range []struct{ label, value, field string }{{"등록 세션", "3", "Registered"}, {"수신 준비", "1", "Ready"}, {"확인 불가", "1", "Unreachable"}, {"중지", "1", "Suspended"}, {"등록 없음", "1", "Reserved"}} {
		fragment := `<span class="metric__label">` + stat.label + `</span><strong class="metric__value" data-session-summary="` + stat.field + `">` + stat.value + `</strong>`
		if !strings.Contains(manage, fragment) {
			t.Errorf("count %s=%s is outside project management card", stat.label, stat.value)
		}
	}
	if strings.Contains(body, `class="metrics"`) || strings.Contains(body, `class="metric"`) {
		t.Error("large session count card area remains")
	}
	for _, link := range []string{`href="/projects/settings?project=demo"`, `href="/auth/code"`, `href="/profile"`} {
		if !strings.Contains(manage, link) {
			t.Errorf("management link changed: %s", link)
		}
	}
}
