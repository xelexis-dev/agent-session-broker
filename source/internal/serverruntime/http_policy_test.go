package serverruntime

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestHTTPProjectPolicyControlsApprovedSessionsWithoutRevokingThem(t *testing.T) {
	r, projectToken := newDeviceRuntime(t)
	markHTTP := func() http.Handler {
		original := r.h
		r.h = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Header.Set("X-ASB-Client-Transport", "http")
			original.ServeHTTP(w, req)
		})
		return original
	}
	original := markHTTP()
	policy := func(want bool) {
		t.Helper()
		w := r.browser(http.MethodGet, "/oauth/transport-policy?project=test", nil)
		var result struct {
			Project string `json:"project"`
			Allow   bool   `json:"allow_http"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Project != "test" || result.Allow != want {
			t.Fatalf("policy status=%d body=%s", w.Code, w.Body.String())
		}
	}
	toggle := func(value string) {
		t.Helper()
		w := r.browser(http.MethodPost, "/admin/http-policy", url.Values{"allowHTTP": {value}})
		if w.Code != 303 {
			t.Fatalf("policy mutation: %d %s", w.Code, w.Body.String())
		}
	}
	policy(false)
	if status, body := r.rest("/oauth/device_authorization?project=test", projectToken, url.Values{}); status != 403 || body["error"] != "http_not_allowed" {
		t.Fatalf("default HTTP access not denied: %d %v", status, body)
	}
	toggle("true")
	policy(true)
	first, second := protectedRequest(1, "ignored"), protectedRequest(2, "ignored")
	firstToken := r.authorize(projectToken, first, "http-one")
	secondToken := r.authorize(projectToken, second, "http-two")
	for _, item := range []struct {
		token string
		req   broker.RegisterRequest
	}{{firstToken, first}, {secondToken, second}} {
		result := managedTool[broker.RegisterResult](t, r.bound(item.token), "register_session", item.req)
		if result.Code != "" {
			t.Fatalf("HTTP registration: %+v", result)
		}
	}
	sent := managedTool[broker.SendResult](t, r.bound(firstToken), "send_message", broker.Message{From: "http-one", To: "http-two", Body: "policy-test"})
	if !sent.Accepted {
		t.Fatalf("send: %+v", sent)
	}
	received := managedTool[broker.Message](t, r.bound(secondToken), "wait_for_message", map[string]string{"recipient": "http-two", "channelId": second.ChannelID})
	if received.Body != "policy-test" {
		t.Fatalf("received: %+v", received)
	}
	toggle("false")
	policy(false)
	if status := r.mcpStatus(firstToken); status != 403 {
		t.Fatalf("HTTP credential bypassed disabled policy: %d", status)
	}
	// Disabling HTTP does not revoke the credential or break older HTTPS proxies.
	r.h = original
	listed := managedTool[[]broker.Session](t, r.bound(firstToken), "list_sessions", map[string]any{})
	if len(listed) != 2 {
		t.Fatalf("credential lost after HTTP policy change: %+v", listed)
	}
	markHTTP()
	toggle("true")
	r.restart()
	markHTTP()
	policy(true)
	listed = managedTool[[]broker.Session](t, r.bound(firstToken), "list_sessions", map[string]any{})
	if len(listed) != 2 {
		t.Fatalf("policy/credentials lost after restart: %+v", listed)
	}
}
