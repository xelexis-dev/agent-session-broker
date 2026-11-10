package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestDeviceApprovalReadyLinkNewAndOldServer(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "old", true: "new"}[modern], func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			auth.setCode(func(w http.ResponseWriter, r *http.Request) {
				v := map[string]any{"device_code": "synthetic-device", "user_code": "ABCD-EFGH", "verification_uri": "/auth/code", "expires_in": 600, "interval": 5}
				if modern {
					v["verification_uri_complete"] = "/auth/code?code=ABCD-EFGH"
				}
				writeJSON(w, 200, v)
			})
			a := &sessionAuth{state: clientstate.SessionState{ClientID: "pc-test", Registration: clientstate.Registration{InitialAttempt: initialPending}}}
			opts := durableOptions{device: &deviceAuthOptions{authorizer: auth.client(), nickname: "flag-worker", persist: func(*clientstate.Store, string, clientstate.SessionState) error { return nil }}}
			if e := opts.requestDeviceCode(context.Background(), a, broker.AttachRequest{Harness: "claude", SessionID: "sid", Name: `worker<&+>`, CWD: "/tmp/my-project"}); e != nil {
				t.Fatal(e)
			}
			out := a.report(false, "", "")
			raw, _ := json.Marshal(out)
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			link, _ := fields["verificationUriComplete"].(string)
			u, e := url.Parse(link)
			if e != nil || u.Host == "" || u.Query().Get("code") != "ABCD-EFGH" || u.Query().Get("name") != `worker<&+>` {
				t.Errorf("ready link missing/incorrect encoding")
			}
			if !strings.Contains(out.Message, link) || link == "" {
				t.Error("pending guidance lacks ready link")
			}
			if out.UserCode != "ABCD-EFGH" || out.VerificationURI != auth.server.URL+"/auth/code" {
				t.Error("existing fields changed")
			}
			if strings.Contains(link, "synthetic-device") || strings.Contains(link, "asb_p_") {
				t.Error("secret in link")
			}
		})
	}
}

func TestDeviceApprovalLinkHintsAndOrigin(t *testing.T) {
	for _, name := range []string{"flag-worker", ""} {
		auth := newDeviceAuthServer(t)
		a := &sessionAuth{state: clientstate.SessionState{ClientID: "pc-test", Registration: clientstate.Registration{InitialAttempt: initialPending}}}
		opts := durableOptions{device: &deviceAuthOptions{authorizer: auth.client(), nickname: name, persist: func(*clientstate.Store, string, clientstate.SessionState) error { return nil }}}
		if e := opts.requestDeviceCode(t.Context(), a, broker.AttachRequest{Harness: "claude", SessionID: "sid", CWD: `C:\work\project`}); e != nil {
			t.Fatal(e)
		}
		u, _ := url.Parse(a.report(false, "", "").VerificationURIComplete)
		if u.Query().Get("name") != name {
			t.Error("flag/no-name fallback changed")
		}
		_, form := auth.last()
		if form["cwd_basename"] != "project" {
			t.Error("full cwd sent or Windows basename not normalized")
		}
		writes := 0
		opts.device.persist = func(*clientstate.Store, string, clientstate.SessionState) error { writes++; return nil }
		opts.updateApprovalNickname(a, "explicit-later")
		if writes != 0 {
			t.Error("late nickname hint must not race credential persistence")
		}
		u, _ = url.Parse(a.report(false, "", "").VerificationURIComplete)
		if u.Query().Get("name") != "explicit-later" {
			t.Error("pending explicit nickname not updated")
		}
	}
	auth := newDeviceAuthServer(t)
	auth.setCode(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"device_code": "synthetic", "user_code": "CODE", "verification_uri": "/auth/code", "verification_uri_complete": "https://evil.example/auth/code?code=CODE", "expires_in": 600})
	})
	if _, e := auth.client().requestCode(t.Context(), deviceCodeRequest{}); e == nil {
		t.Error("foreign complete URI accepted")
	}
	link := approvalReadyLink("https://broker/auth/code", "https://broker/auth/code?token=secret#fragment", "CODE", "worker&name")
	if strings.Contains(link, "secret") || strings.Contains(link, "fragment") {
		t.Error("arbitrary URI query/fragment propagated")
	}
}
