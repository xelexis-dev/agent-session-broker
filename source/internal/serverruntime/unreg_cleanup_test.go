package serverruntime

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestUnregCleanupU1ListingRevokesOnlyCapableAutoGrant(t *testing.T) {
	var parent string
	r, _ := newDeviceRuntime(t, func(s *controlpanel.Store) {
		if s.SetScopeIssuanceReady("admin", true) != nil {
			t.Fatal("scope gate")
		}
		var err error
		parent, err = s.IssueProjectTokenWithOptionsFor("admin", "test", "unreg-parent", controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
		if err != nil {
			t.Fatal("auto parent")
		}
	})
	request := protectedRequest(1401, "ignored")
	request.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
	status, grant := r.rest("/oauth/session_registration?project=test", parent, url.Values{"client_id": {"asb-client-v1"}, "harness": {request.Harness}, "session_id": {request.SessionID}, "asb_client_instance_id": {request.ClientID}, "request_id": {request.RequestID}, "requested_nickname": {"unreg-worker"}})
	token, _ := grant["access_token"].(string)
	if status != 200 || token == "" {
		t.Fatal("direct issuance", status)
	}
	registered := managedTool[broker.RegisterResult](t, r.bound(token), "register_session", request)
	if registered.Code != "" {
		t.Fatal("auto registration", registered.Code)
	}
	managedTool[any](t, r.bound(token), "stop_session", map[string]string{"name": registered.Session.RegistrationID})
	page := r.browser(http.MethodGet, "/projects/sessions?project=test", nil)
	if page.Code != 200 {
		t.Fatal("panel listing", page.Code)
	}
	if r.mcpStatus(token) != 401 {
		t.Fatal("U1: detached capable auto grant remains active")
	}
	if strings.Contains(page.Body.String(), "unreg-worker") {
		t.Fatal("U1: unregistered row remained")
	}
}

func TestUnregCleanupManualAndOldCurrentHolderKeepReservation(t *testing.T) {
	for _, mode := range []string{"manual", "no-capability", "old-resume"} {
		t.Run(mode, func(t *testing.T) {
			r, parent := unregRuntime(t)
			request := protectedRequest(1451, "unreg-preserved")
			var token string
			var out broker.RegisterResult
			if mode == "manual" {
				token = r.authorize(parent, request, "unreg-preserved")
				request.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
				out = managedTool[broker.RegisterResult](t, r.bound(token), "register_session", request)
			} else {
				token, out = unregAuto(t, r, parent, 1451, "unreg-preserved", mode == "old-resume")
			}
			if out.Code != "" {
				t.Fatal("fixture registration", out.Code)
			}
			if mode == "old-resume" {
				resume := broker.ResumeRequest{Holder: request.Holder, RegistrationID: out.Session.RegistrationID, Harness: request.Harness, SessionID: request.SessionID, PID: request.PID, CWD: request.CWD, ClientID: request.ClientID, ChannelID: "old-client-resume", Secret: request.Secret, RequestID: "old-client-resume", ExpectedRevision: out.Session.Revision}
				out = managedTool[broker.RegisterResult](t, r.bound(token), "resume_session", resume)
				if out.Code != "" {
					t.Fatal("old holder resume", out.Code)
				}
			}
			managedTool[any](t, r.bound(token), "stop_session", map[string]string{"name": out.Session.RegistrationID})
			page := r.browser("GET", "/projects/sessions?project=test", nil)
			if page.Code != 200 || r.mcpStatus(token) == 401 || !strings.Contains(page.Body.String(), "unreg-preserved") {
				t.Fatal("manual/old holder lost its reserved credential")
			}
		})
	}
}

func TestUnregCleanupOtherCallerAndSurvivingSession(t *testing.T) {
	r, parent := unregRuntime(t)
	a, manager := unregAuto(t, r, parent, 1461, "survivor", true)
	target, b := unregAuto(t, r, parent, 1462, "cleanup-target", true)
	managedTool[any](t, r.bound(a), "stop_session", map[string]string{"name": b.Session.RegistrationID})
	page := r.browser("GET", "/projects/sessions?project=test", nil)
	if page.Code != 200 || r.mcpStatus(target) != 401 || r.mcpStatus(a) == 401 || strings.Contains(page.Body.String(), "cleanup-target") {
		t.Fatal("other-caller cleanup affected survivor")
	}
	got := managedTool[broker.Session](t, r.bound(a), "get_session", map[string]string{"name": manager.Session.RegistrationID})
	if got.RegistrationID != manager.Session.RegistrationID || !got.AutomaticDeliveryReady {
		t.Fatal("surviving registration changed")
	}
}
