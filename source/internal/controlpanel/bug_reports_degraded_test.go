package controlpanel

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
)

func TestBugReportUnavailableAdminEndpointsAreFixed503(t *testing.T) {
	c := loginPanelUser(t, bugReportTestHandler(t, &fakeBugReports{err: bugreport.ErrUnavailable}), "admin")
	for _, path := range []string{"/admin/bug-reports", "/admin/bug-reports/detail?id=bug-demo-004"} {
		out := c.call("GET", path, nil)
		if out.Code != http.StatusServiceUnavailable || strings.TrimSpace(out.Body.String()) != bugreport.ErrUnavailable.Error() {
			t.Fatal("admin unavailable GET is not a fixed 503")
		}
	}
	out := c.call("POST", "/admin/bug-reports/status", url.Values{"id": {"bug-demo-004"}, "status": {"resolved"}, "expectedVersion": {"fixture-version"}})
	if out.Code != 503 || strings.TrimSpace(out.Body.String()) != bugreport.ErrUnavailable.Error() {
		t.Fatal("admin unavailable status is not a fixed 503")
	}
	for _, user := range []string{"owner", "developer", "viewer"} {
		other := loginPanelUser(t, bugReportTestHandler(t, &fakeBugReports{err: bugreport.ErrUnavailable}), user)
		if other.call("GET", "/admin/bug-reports", nil).Code != 403 {
			t.Fatal("unavailable state bypassed administrator boundary")
		}
	}
}
