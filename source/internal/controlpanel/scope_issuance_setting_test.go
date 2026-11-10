package controlpanel

import (
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestScopeIssuanceAdminPanelEnablesAutoAndDiscovery(t *testing.T) {
	s := userStore(t)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	owner := loginPanelUser(t, h, "owner")
	form := url.Values{"project": {"demo"}, "label": {"auto"}, "scope_selection": {"custom"}, "scope": {access.SessionRegister, access.SessionRegisterAuto}}
	if w := owner.call("POST", "/tokens/project", form); w.Code != 400 {
		t.Fatal("default gate permits auto")
	}
	if w := owner.call("POST", "/admin/scope-issuance", url.Values{"enabled": {"true"}}); w.Code != 403 {
		t.Errorf("non-admin POST status=%d want403", w.Code)
	}
	if w := admin.call("POST", "/admin/scope-issuance", url.Values{"enabled": {"true"}}); w.Code != 303 {
		t.Fatalf("admin enable POST status=%d want303", w.Code)
	}
	if w := owner.call("POST", "/tokens/project", form); w.Code != 303 {
		t.Fatalf("enabled auto issuance status=%d want303", w.Code)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://broker/oauth/capabilities", nil))
	if !strings.Contains(w.Body.String(), "direct-registration-v1") {
		t.Fatal("enabled discovery lacks direct")
	}
}

func TestScopeIssuanceOffKeepsScopesAndBlocksDirect(t *testing.T) {
	s := directStore(t)
	// Start at the real default before enabling through the public store method.
	s.data.ScopeIssuanceReady = false
	if s.SetScopeIssuanceReady("admin", true) != nil {
		t.Fatal("enable")
	}
	auto := directParent(t, s, 8)
	subset, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "restricted", ProjectTokenOptions{Scopes: []string{access.SessionRegister}})
	if err != nil {
		t.Fatal("restricted issuance")
	}
	before := s.data.Projects["demo"].Tokens
	if s.SetScopeIssuanceReady("admin", false) != nil {
		t.Fatal("disable")
	}
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	if w := directPOST(t, h, auto, directForm()); w.Code != 403 {
		t.Fatal("OFF allowed direct exchange")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://broker/oauth/capabilities", nil))
	if strings.Contains(w.Body.String(), "direct-registration-v1") || !strings.Contains(w.Body.String(), "scoped-session-v1") {
		t.Fatal("OFF discovery changed scope enforcement contract")
	}
	if _, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "blocked", ProjectTokenOptions{Scopes: []string{access.SessionRegister}}); err == nil {
		t.Fatal("OFF issued restricted token")
	}
	after := s.data.Projects["demo"].Tokens
	_, _, parentErr := s.projectTokenLocked("demo", subset)
	if !reflect.DeepEqual(before, after) || parentErr != nil {
		t.Fatal("OFF changed existing credentials")
	}
}

func TestScopeIssuancePersistenceAuditAndRollback(t *testing.T) {
	dir := t.TempDir()
	open := OpenSQLite
	s, err := open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Setup(testPassword) != nil {
		t.Fatal("setup")
	}
	if ready, last := s.scopeIssuancePolicy(); ready || last != nil {
		t.Fatal("old/new DB not default OFF")
	}
	saved := s.write
	s.write = func([]byte) error { return errors.New("injected") }
	if s.SetScopeIssuanceReady("admin", true) == nil {
		t.Fatal("write failure accepted")
	}
	if ready, last := s.scopeIssuancePolicy(); ready || last != nil {
		t.Fatal("failed write published policy/audit")
	}
	s.write = saved
	if s.SetScopeIssuanceReady("admin", true) != nil {
		t.Fatal("enable")
	}
	if s.SetScopeIssuanceReady("admin", true) != nil || len(s.data.ScopeIssuanceChanges) != 1 {
		t.Fatal("no-op generated audit")
	}
	if s.SetScopeIssuanceReady("admin", false) != nil {
		t.Fatal("disable")
	}
	s.Close()
	s, err = open(dir)
	if err != nil {
		t.Fatal("restart")
	}
	defer s.Close()
	ready, last := s.scopeIssuancePolicy()
	if ready || last == nil || last.Actor != "admin" || !bool(last.Old) || bool(last.New) || last.At.IsZero() || len(s.data.ScopeIssuanceChanges) != 2 {
		t.Fatal("audit/policy lost on restart")
	}
}

func TestScopeIssuanceStrictState(t *testing.T) {
	s := userStore(t)
	raw, _ := json.Marshal(s.data)
	var obj map[string]json.RawMessage
	json.Unmarshal(raw, &obj)
	for _, bad := range []string{`null`, `"true"`, `1`, `{}`} {
		obj["scopeIssuanceReady"] = json.RawMessage(bad)
		b, _ := json.Marshal(obj)
		var target state
		if decodeControlState(b, &target) == nil {
			t.Errorf("invalid policy accepted: %s", bad)
		}
	}
	delete(obj, "scopeIssuanceReady")
	for _, bad := range []string{`[null]`, `[{"actor":"admin","at":"2026-10-14T00:00:00Z","new":true}]`, `[{"actor":"admin","at":"2026-10-14T00:00:00Z","old":false,"new":false}]`} {
		obj["scopeIssuanceChanges"] = json.RawMessage(bad)
		b, _ := json.Marshal(obj)
		var target state
		if decodeControlState(b, &target) == nil {
			t.Fatal("invalid audit accepted")
		}
	}
}

func TestScopeIssuancePanelGuardsAndCapture(t *testing.T) {
	s := userStore(t)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	for _, bad := range []url.Values{{}, {"enabled": {"yes"}}, {"enabled": {"true", "false"}}, {"enabled": {"true"}, "extra": {"x"}}} {
		if w := admin.call("POST", "/admin/scope-issuance", bad); w.Code != 400 {
			t.Fatal("invalid form accepted")
		}
	}
	req := httptest.NewRequest("POST", "http://broker/admin/scope-issuance", strings.NewReader("enabled=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(admin.cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if w := admin.call("GET", "/admin/scope-issuance", nil); w.Code != 405 {
		t.Fatal("GET mutates")
	}
	capture := func(name string) {
		w := admin.call("GET", "/admin/settings", nil)
		for _, text := range []string{"클라이언트 0.15", "끄면 새 제한·자동 토큰 발급과 자동 등록을 막습니다. 기존 토큰은 유지됩니다.", "data-confirm-action=\"scope-issuance\""} {
			if !strings.Contains(w.Body.String(), text) {
				t.Fatal("settings explanation absent")
			}
		}
		if dir := os.Getenv("ASB_SCOPE_CAPTURE_DIR"); dir != "" {
			os.MkdirAll(dir, 0700)
			if os.WriteFile(filepath.Join(dir, name+".html"), w.Body.Bytes(), 0600) != nil {
				t.Fatal("capture fixture")
			}
		}
	}
	capture("off")
	if w := admin.call("POST", "/admin/scope-issuance", url.Values{"enabled": {"true"}}); w.Code != 303 {
		t.Fatal("enable")
	}
	capture("on")
	if !strings.Contains(admin.call("GET", "/admin/settings", nil).Body.String(), "OFF → ON") {
		t.Fatal("last change not shown")
	}
}

// Synthetic control state only; opt-in fixture for an actual older server binary.
func TestScopeIssuanceDowngradeFixtures(t *testing.T) {
	dir := os.Getenv("ASB_SCOPE_DOWNGRADE_DIR")
	if dir == "" {
		t.Skip("opt-in synthetic downgrade fixtures")
	}
	for _, mode := range []string{"never-set", "on", "off-after-on"} {
		s, err := OpenSQLite(filepath.Join(dir, mode, "control"))
		if err != nil {
			t.Fatal("open fixture")
		}
		if s.Setup(testPassword) != nil {
			t.Fatal("setup fixture")
		}
		if mode != "never-set" && s.SetScopeIssuanceReady("admin", true) != nil {
			t.Fatal("enable fixture")
		}
		if mode == "off-after-on" && s.SetScopeIssuanceReady("admin", false) != nil {
			t.Fatal("disable fixture")
		}
		if s.Close() != nil {
			t.Fatal("close fixture")
		}
	}
}
