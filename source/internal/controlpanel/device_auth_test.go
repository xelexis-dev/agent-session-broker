package controlpanel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDeviceAuthorizationTwoSessionsAndReplay(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProject("devices"); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueProjectTokenFor("admin", "devices", "my harnesses")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "asb_p_") || s.Authorize("devices", raw) {
		t.Fatal("project token must not authorize MCP")
	}
	for _, name := range []string{"one", "two"} {
		req := DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: name, InstanceID: "pc-example", RequestID: "request-" + name}
		code, err := s.BeginDeviceAuthorization("devices", raw, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ExchangeDeviceToken("devices", raw, code.DeviceCode, req.ClientID, false); err == nil {
			t.Fatal("pending granted")
		}
		if _, err = s.ApproveDeviceAuthorization("admin", code.UserCode, name, false); err != nil {
			t.Fatal(err)
		}
		// Poll times are local rate state, not wall-clock sleeps in the test.
		s.mu.Lock()
		s.devicePoll = nil
		s.mu.Unlock()
		first, err := s.ExchangeDeviceToken("devices", raw, code.DeviceCode, req.ClientID, false)
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.devicePoll = nil
		s.mu.Unlock()
		second, err := s.ExchangeDeviceToken("devices", raw, code.DeviceCode, req.ClientID, false)
		if err != nil {
			t.Fatal(err)
		}
		if first.AccessToken == second.AccessToken || s.Authorize("devices", first.AccessToken) {
			t.Fatal("unused exchange did not replace credential")
		}
		grant, _, ok := s.authorize("devices", second.AccessToken)
		if !ok || grant.Nickname != name {
			t.Fatal("missing approved session")
		}
		identity, _ := json.Marshal([3]string{req.Harness, req.SessionID, req.InstanceID})
		if grant.Claim(string(identity)) != nil || grant.Check("other") == nil {
			t.Fatal("identity isolation failed")
		}
		s.mu.Lock()
		s.devicePoll = nil
		s.mu.Unlock()
		if _, err = s.ExchangeDeviceToken("devices", raw, code.DeviceCode, req.ClientID, false); err == nil {
			t.Fatal("used credential reissued")
		}
	}
	_ = time.Second
}
