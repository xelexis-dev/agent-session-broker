package controlpanel

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestApproveDeviceAuthorizationTrimsSurroundingWhitespace(t *testing.T) {
	f := newDeviceFixture(t)
	req := DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "trim-session", InstanceID: "pc-example", RequestID: "trim-request"}
	code, err := f.store.BeginDeviceAuthorization(f.project, f.raw, req)
	if err != nil {
		t.Fatal("synthetic authorization setup failed")
	}
	approved, err := f.store.ApproveDeviceAuthorization("owner", "  "+code.UserCode+"  ", "  agent-one  ", false)
	if err != nil {
		t.Fatalf("surrounding whitespace should be trimmed before approval: %v", err)
	}
	if approved.Nickname != "agent-one" {
		t.Fatal("approved nickname was not trimmed")
	}
	credential, err := f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false)
	if err != nil || credential.Nickname != "agent-one" {
		t.Fatal("stored/exchanged nickname was not trimmed")
	}
}

func TestApprovalTrimPreservesNicknameValidation(t *testing.T) {
	for _, nickname := range []string{"  agent one  ", "  Broker-Admin  ", "  name:other  ", "   "} {
		t.Run(strings.TrimSpace(nickname), func(t *testing.T) {
			f := newDeviceFixture(t)
			code, err := f.store.BeginDeviceAuthorization(f.project, f.raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "invalid-trim", InstanceID: "pc-example", RequestID: "invalid-trim-request"})
			if err != nil {
				t.Fatal("synthetic setup failed")
			}
			if _, err := f.store.ApproveDeviceAuthorization("owner", code.UserCode, nickname, false); !errors.Is(err, ErrInvalid) {
				t.Fatal("trim bypassed nickname validation")
			}
			view, err := f.store.DeviceAuthorizationFor("owner", code.UserCode)
			if err != nil || view.Status != "pending" {
				t.Fatal("invalid nickname changed approval state")
			}
		})
	}
}

func TestApprovalHTTPTrimsNicknameAndRedisplaysTrimmedErrors(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	owner := loginPanelUser(t, f.h, "owner")
	parent, err := f.store.IssueProjectTokenFor("owner", "demo", "trim-parent")
	if err != nil {
		t.Fatal("synthetic parent setup failed")
	}
	for i, want := range []int{303, 409} {
		req := DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: []string{"http-trim-one", "http-trim-two"}[i], InstanceID: "pc-example", RequestID: []string{"trim-request-one", "trim-request-two"}[i]}
		code, err := f.store.BeginDeviceAuthorization("demo", parent, req)
		if err != nil {
			t.Fatal("synthetic code setup failed")
		}
		w := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {"  " + code.UserCode + "  "}, "nickname": {"  agent-one  "}})
		if w.Code != want {
			t.Fatalf("approval status: got %d, want %d", w.Code, want)
		}
		if want == 409 {
			body := w.Body.String()
			if !strings.Contains(body, `name="nickname" value="agent-one"`) || !strings.Contains(body, "닉네임 agent-one은(는)") || strings.Contains(body, "  agent-one  ") {
				t.Fatal("duplicate error redisplayed untrimmed nickname")
			}
		} else {
			view, err := f.store.DeviceAuthorizationFor("owner", code.UserCode)
			if err != nil || view.Nickname != "agent-one" {
				t.Fatal("HTTP approval did not store trimmed nickname")
			}
		}
	}
}
