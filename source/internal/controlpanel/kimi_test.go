package controlpanel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestKimiDirectAuthorizationSurvivesRestart(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 8)
	form := directForm()
	form.Set("harness", "kimi")
	req := directRequest(form)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	w := directPOST(t, h, parent, form)
	if w.Code != 200 {
		t.Fatalf("Kimi direct status=%d", w.Code)
	}
	var result DeviceTokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.AccessToken == "" {
		t.Fatal("invalid Kimi direct response")
	}
	grant := directGrant(t, s, result.AccessToken)
	if grant.Claim(req.identity()) != nil || grant.Check(req.identity()) != nil {
		t.Fatal("Kimi direct binding rejected")
	}
	if grant.Check(DeviceRequest{Harness: "codex", SessionID: req.SessionID, InstanceID: req.InstanceID}.identity()) == nil {
		t.Fatal("cross-harness binding accepted")
	}
	root := s.root.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	grant = directGrant(t, restored, result.AccessToken)
	if grant.Check(req.identity()) != nil {
		t.Fatal("Kimi direct binding lost after restart")
	}
}

func TestKimiDeviceAuthorizationSurvivesRestart(t *testing.T) {
	s := directStore(t)
	raw, err := s.IssueProjectTokenFor("admin", "demo", "Kimi device")
	if err != nil {
		t.Fatal(err)
	}
	req := DeviceRequest{ClientID: "asb-client-v1", Harness: "kimi", SessionID: "11111111-1111-4111-8111-111111111111", InstanceID: "client-kimi-0001", RequestID: "kimi-device"}
	code, err := s.BeginDeviceAuthorization("demo", raw, req)
	if err != nil {
		t.Fatal("Kimi device request rejected", err)
	}
	root := s.root.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	view, err := s.DeviceAuthorizationFor("admin", code.UserCode)
	if err != nil || view.Harness != "kimi" {
		t.Fatal("Kimi device request lost after restart", err)
	}
	if _, err := s.ApproveDeviceAuthorization("admin", code.UserCode, "kimi-worker", false); err != nil {
		t.Fatal(err)
	}
	token, err := s.ExchangeDeviceToken("demo", raw, code.DeviceCode, req.ClientID, false)
	if err != nil || token.AccessToken == "" {
		t.Fatal("Kimi device exchange failed", err)
	}
	grant := directGrant(t, s, token.AccessToken)
	if grant.Claim(req.identity()) != nil || grant.Check(req.identity()) != nil {
		t.Fatal("Kimi device binding rejected")
	}
}

func TestKimiUnknownAuthorizationHarnessRejected(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 8)
	req := directRequest(directForm())
	req.Harness = "other"
	if _, err := s.ExchangeDirect("demo", parent, req, DirectProof{}, "peer", false); err == nil {
		t.Fatal("unknown direct harness accepted")
	}
	if _, err := s.BeginDeviceAuthorization("demo", parent, req.DeviceRequest); err == nil {
		t.Fatal("unknown device harness accepted")
	}
	if canonicalDirectIdentity(req.identity()) {
		t.Fatal("unknown direct identity accepted")
	}
}

func TestKimiCapabilityAdvertisedRegardlessOfDirectGate(t *testing.T) {
	s := directStore(t)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	for _, enabled := range []bool{false, true} {
		s.data.ScopeIssuanceReady = policyBool(enabled)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/capabilities", nil))
		var result struct {
			Capabilities []string `json:"capabilities"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !slices.Contains(result.Capabilities, "harness-kimi-v1") {
			t.Fatalf("Kimi discovery absent: direct enabled=%v", enabled)
		}
	}
}

type kimiPanelBackend struct{ layoutBackend }

func (kimiPanelBackend) Sessions(context.Context, string) ([]Session, error) {
	return []Session{{ID: "kimi-registration", SessionID: "11111111-1111-4111-8111-111111111111", ClientID: "client-kimi-0001", Name: "kimi-worker", Harness: "kimi", Status: "alive", Ready: true}}, nil
}
func TestKimiPanelRendersHarnessName(t *testing.T) {
	h := New(userStore(t), kimiPanelBackend{}, http.NotFoundHandler())
	page := loginPanelUser(t, h, "owner").call("GET", "/projects/sessions?project=demo", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `data-label="하네스">kimi</td>`) {
		t.Fatal("Kimi harness name missing from panel")
	}
}
