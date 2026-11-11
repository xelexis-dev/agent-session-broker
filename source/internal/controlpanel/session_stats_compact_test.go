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
	for _, stat := range []struct{ label, value, field string }{{en("sessions.metric_registered"), "3", "Registered"}, {en("session.connection.ready"), "1", "Ready"}, {en("session.connection.unreachable"), "1", "Unreachable"}, {en("session.connection.suspended"), "1", "Suspended"}, {en("session.connection.unregistered"), "1", "Reserved"}} {
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
