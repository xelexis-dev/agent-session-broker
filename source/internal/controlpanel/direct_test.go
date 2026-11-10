package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func directStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if s.Setup(testPassword) != nil || s.CreateProject("demo") != nil {
		t.Fatal("fixture setup")
	}
	s.data.ScopeIssuanceReady = true
	return s
}
func directPOST(t *testing.T, h http.Handler, raw string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "https://broker/oauth/session_registration?project=demo", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func directForm() url.Values {
	return url.Values{"client_id": {"asb-client-v1"}, "harness": {"codex"}, "session_id": {"11111111-1111-4111-8111-111111111111"}, "asb_client_instance_id": {"client-direct-test"}, "request_id": {"direct-request"}, "requested_nickname": {"worker"}}
}
func TestDirectEndpointRequiresAutoAndCreatesSingleBoundChild(t *testing.T) {
	s := directStore(t)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	ordinary, err := s.IssueProjectTokenFor("admin", "demo", "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	w := directPOST(t, h, ordinary, directForm())
	if w.Code != 403 {
		t.Errorf("ordinary parent status=%d want403", w.Code)
	}
	for _, token := range s.data.Projects["demo"].Tokens {
		if token.Kind != tokenProject {
			t.Fatal("ordinary parent created grant/reservation")
		}
	}
	auto, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "auto", ProjectTokenOptions{Scopes: []string{access.SessionRegister, access.SessionRegisterAuto, access.SessionRead}})
	if err != nil {
		t.Fatal(err)
	}
	w = directPOST(t, h, auto, directForm())
	if w.Code != 200 {
		t.Fatalf("auto parent status=%d want200", w.Code)
	}
	var result DeviceTokenResponse
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.AccessToken == "" || result.Nickname != "worker" {
		t.Fatal("invalid direct response")
	}
	children := 0
	for _, token := range s.data.Projects["demo"].Tokens {
		if token.Kind == tokenSession {
			children++
			if token.BoundIdentity == "" || access.HasScope(token.Scopes, access.SessionRegisterAuto) {
				t.Fatal("child binding/delegation invalid")
			}
		}
	}
	if children != 1 || len(s.data.Projects["demo"].Devices) != 0 {
		t.Fatal("direct exchange not exactly one child without device approval")
	}
}

func TestDirectFullGrantWireKeepsReleasedClientUnrestricted(t *testing.T) {
	s := directStore(t)
	scopes := append(access.LegacyV1Scopes(), access.SessionRegisterAuto)
	slices.Sort(scopes)
	parent, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "auto-full", ProjectTokenOptions{Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	w := directPOST(t, New(s, tokenViewBackend{}, http.NotFoundHandler()), parent, directForm())
	var out DirectResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("full grant exchange failed")
	}
	// Released clients use this count to classify an approved full grant.
	if len(out.Scopes) != 9 || !access.ValidScopes(out.Scopes) || !access.HasScope(out.Scopes, access.SessionSpawn) {
		t.Fatal("full direct grant looks restricted to released clients")
	}
	for _, token := range s.data.Projects["demo"].Tokens {
		if access.HasScope(token.Scopes, access.SessionSpawn) {
			t.Fatal("compatibility scope persisted")
		}
	}
}
