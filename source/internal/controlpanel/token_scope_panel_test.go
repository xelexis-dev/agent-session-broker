package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestScopedTokenPanelIssuerAndSnapshot(t *testing.T) {
	s := userStore(t)
	s.data.ScopeIssuanceReady = true
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	dev := loginPanelUser(t, h, "developer")
	before := len(s.ProjectsFor("owner")[0].Tokens)
	w := dev.call("POST", "/tokens/project", url.Values{"project": {"demo"}, "label": {"forged-auto"}, "scope_selection": {"custom"}, "scope": {access.SessionRegister, access.SessionRegisterAuto}})
	if w.Code != http.StatusForbidden {
		t.Errorf("developer auto request status=%d, want 403", w.Code)
	}
	if len(s.ProjectsFor("owner")[0].Tokens) != before {
		t.Error("developer auto request created a token")
	}
	w = dev.call("POST", "/tokens/project", url.Values{"project": {"demo"}, "label": {"read-only"}, "scope_selection": {"custom"}, "scope": {access.SessionRegister, access.SessionRead}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("ordinary subset status=%d, want 303", w.Code)
	}
	var found bool
	for _, token := range s.ProjectsFor("owner")[0].Tokens {
		if token.Label == "read-only" {
			found = true
			if !slices.Equal(token.Scopes, []string{access.SessionRead, access.SessionRegister}) {
				t.Error("ordinary subset was not stored exactly")
			}
		}
	}
	if !found {
		t.Fatal("subset token missing")
	}
	page := dev.call("GET", "/projects/settings?project=demo", nil).Body.String()
	if !strings.Contains(page, `data-token-scope="session:read"`) {
		t.Error("stored scopes are not shown in token list")
	}
}

func TestScopedTokenIssuanceGateAndPanelRoles(t *testing.T) {
	s := userStore(t)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	form := url.Values{"project": {"demo"}, "label": {"blocked"}, "scope_selection": {"custom"}, "scope": {access.SessionRegister}}
	if w := owner.call("POST", "/tokens/project", form); w.Code != 400 || !strings.Contains(w.Body.String(), tokenScopeUpgradeMessage) {
		t.Fatal("gate-off subset not rejected with upgrade guidance")
	}
	if len(s.ProjectsFor("owner")[0].Tokens) != 0 {
		t.Fatal("gate-off created token")
	}
	body := owner.call("GET", "/projects/settings?project=demo", nil).Body.String()
	if !strings.Contains(body, `class="token-scope-options" disabled`) || strings.Contains(body, `name="scope_selection"`) {
		t.Fatal("gate-off form can select restricted scopes")
	}
	if _, err := s.IssueProjectTokenFor("developer", "demo", "legacy"); err != nil {
		t.Fatal(err)
	}
	token := s.ProjectsFor("owner")[0].Tokens[0]
	if !slices.Equal(token.Scopes, access.LegacyV1Scopes()) || token.AdmissionMode != "manual" || !token.ExpiresAt.IsZero() || token.MaxAutoBindings != 0 {
		t.Fatal("default issuance changed")
	}
	s.data.ScopeIssuanceReady = true
	dev := loginPanelUser(t, h, "developer")
	body = dev.call("GET", "/projects/settings?project=demo", nil).Body.String()
	if !strings.Contains(body, `data-auto-scope disabled`) {
		t.Fatal("developer auto control enabled")
	}
	viewer := loginPanelUser(t, h, "viewer")
	body = viewer.call("GET", "/projects/settings?project=demo", nil).Body.String()
	if strings.Contains(body, `action="/tokens/project"`) || strings.Contains(body, `action="/tokens/revoke"`) {
		t.Fatal("viewer has mutation controls")
	}
	if w := viewer.call("POST", "/tokens/project", form); w.Code != 403 {
		t.Fatal("viewer crafted issuance accepted")
	}
	req := httptest.NewRequest("POST", "http://broker/tokens/project", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(owner.cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("issuance without CSRF accepted")
	}
}

func TestScopedTokenAutoBoundsAndPersistence(t *testing.T) {
	s := userStore(t)
	s.data.ScopeIssuanceReady = true
	now := time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	for _, tc := range []struct {
		cap, days string
		status    int
	}{
		{"", "", 303}, {"1", "1", 303}, {"64", "90", 303}, {"65", "91", 303}, {"100000", "36500", 303}, {"unlimited", "unlimited", 303}, {"unlimited", "30", 303}, {"8", "unlimited", 303},
		{"0", "30", 303}, {"100001", "30", 400}, {"8", "0", 303}, {"8", "36501", 400}, {"-1", "30", 400}, {"8", "invalid", 400}, {"8", "-1", 400}, {"bad", "30", 400},
	} {
		form := url.Values{"project": {"demo"}, "label": {"auto-example"}, "scope_selection": {"custom"}, "scope": {access.SessionRegisterAuto, access.SessionRegister}}
		if tc.cap != "" {
			form.Set("auto_cap", tc.cap)
		}
		if tc.days != "" {
			form.Set("auto_days", tc.days)
		}
		before := len(s.ProjectsFor("owner")[0].Tokens)
		w := owner.call("POST", "/tokens/project", form)
		if w.Code != tc.status {
			t.Fatalf("auto cap=%s days=%s status=%d", tc.cap, tc.days, w.Code)
		}
		after := len(s.ProjectsFor("owner")[0].Tokens)
		if tc.status != 303 {
			if after != before {
				t.Fatal("invalid auto created token")
			}
			continue
		}
		token := s.ProjectsFor("owner")[0].Tokens[after-1]
		cap, days := 8, 30
		if tc.cap != "" {
			cap, _ = strconv.Atoi(tc.cap)
		}
		if tc.days != "" {
			days, _ = strconv.Atoi(tc.days)
		}
		expires := now.Add(time.Duration(days) * 24 * time.Hour)
		if tc.days == "unlimited" || tc.days == "0" {
			expires = time.Time{}
		}
		if token.AdmissionMode != "auto" || token.IssuedBy != "owner" || token.MaxAutoBindings != cap || !token.ExpiresAt.Equal(expires) {
			t.Fatal("auto metadata mismatch")
		}
		first := owner.call("GET", w.Header().Get("Location"), nil)
		if first.Header().Get("Cache-Control") != "no-store" || !strings.Contains(first.Body.String(), `id="issued-token"`) {
			t.Fatal("one-time secret result missing")
		}
		again := owner.call("GET", w.Header().Get("Location"), nil)
		if strings.Contains(again.Body.String(), `id="issued-token"`) {
			t.Fatal("secret result replayed")
		}
	}
	body := owner.call("GET", "/projects/settings?project=demo", nil).Body.String()
	for _, want := range []string{`data-token-scope="session:register:auto"`, "자동 등록", "identity 한도 8개", "만료"} {
		if !strings.Contains(body, want) {
			t.Error("auto list metadata missing")
		}
	}
	input := []string{access.SessionRegister, access.MemoryWrite}
	if _, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "snapshot", ProjectTokenOptions{Scopes: input}); err != nil {
		t.Fatal(err)
	}
	input[0] = access.SessionRegisterAuto
	tokens := s.ProjectsFor("owner")[0].Tokens
	if !slices.Equal(tokens[len(tokens)-1].Scopes, []string{access.MemoryWrite, access.SessionRegister}) {
		t.Fatal("scope snapshot aliases caller")
	}
	dir := s.root.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal("issued scoped metadata could not reload")
	}
	defer reopened.Close()
	if !reopened.data.ScopeIssuanceReady {
		t.Fatal("issuance setting lost on restart")
	}
	if len(reopened.ProjectsFor("owner")[0].Tokens) != len(tokens) {
		t.Fatal("scope snapshots lost on restart")
	}
}

func TestScopedTokenInvalidSelectionsHaveNoSideEffects(t *testing.T) {
	s := userStore(t)
	s.data.ScopeIssuanceReady = true
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	for _, changes := range []url.Values{
		{"scope": {}}, {"scope": {access.MemoryRead}}, {"scope": {access.SessionRegister, access.SessionRegister}},
		{"scope": {access.SessionRegister, "all"}}, {"scope": {""}},
		{"auto_cap": {"8"}}, {"auto_cap": {"unlimited"}}, {"auto_days": {"unlimited"}}, {"auto_cap": {""}}, {"auto_days": {""}}, {"auto_cap": {"8", "unlimited"}}, {"project": {"demo", "demo"}}, {"auto_days": {"30", "40"}}, {"scope_selection": {"custom", "custom"}},
	} {
		form := url.Values{"project": {"demo"}, "label": {"invalid"}, "scope_selection": {"custom"}, "scope": {access.SessionRegister}}
		for k, v := range changes {
			form[k] = v
		}
		if w := owner.call("POST", "/tokens/project", form); w.Code != 400 {
			t.Errorf("invalid scope form status=%d", w.Code)
		}
	}
	if len(s.ProjectsFor("owner")[0].Tokens) != 0 {
		t.Fatal("invalid selections created tokens")
	}
}

func TestAutoAdmissionMetadataFailsClosedOnLoad(t *testing.T) {
	s := userStore(t)
	s.data.ScopeIssuanceReady = true
	if _, err := s.IssueProjectTokenWithOptionsFor("owner", "demo", "auto", ProjectTokenOptions{Scopes: []string{access.SessionRegister, access.SessionRegisterAuto}}); err != nil {
		t.Fatal(err)
	}
	token := s.data.Projects["demo"].Tokens[0]
	for _, mutate := range []func(*SessionToken){
		func(v *SessionToken) { v.MaxAutoBindings = -1 }, func(v *SessionToken) { v.MaxAutoBindings = 100001 },
		func(v *SessionToken) { v.ExpiresAt = v.IssuedAt.Add(-24 * time.Hour) }, func(v *SessionToken) { v.ExpiresAt = v.IssuedAt.Add(36501 * 24 * time.Hour) },
		func(v *SessionToken) { v.AdmissionMode = "manual" }, func(v *SessionToken) { v.IssuedBy = "developer" },
	} {
		v := token
		mutate(&v)
		if validTokenAdmission(v, controlVersion) {
			t.Error("invalid auto metadata load accepted")
		}
	}
	if !validScopeState(s.data) {
		t.Fatal("valid issued auto metadata rejected")
	}
}

func TestAutoParentExpiresAndRetainsHistory(t *testing.T) {
	s := userStore(t)
	s.data.ScopeIssuanceReady = true
	now := time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	raw, err := s.IssueProjectTokenWithOptionsFor("owner", "demo", "finite-auto", ProjectTokenOptions{Scopes: []string{access.SessionRegister, access.SessionRegisterAuto}, AutoLifetimeDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	if _, err = s.BeginDeviceAuthorization("demo", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "synthetic", InstanceID: "pc", RequestID: "expired"}); err == nil {
		t.Fatal("expired auto parent accepted")
	}
	if err = s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	tokens := s.ProjectsFor("owner")[0].Tokens
	if len(tokens) != 1 || tokens[0].ExpiredAt.IsZero() {
		t.Fatal("expired parent history not retained")
	}
	now = now.Add(tokenHistoryRetention)
	if err = s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if len(s.ProjectsFor("owner")[0].Tokens) != 0 {
		t.Fatal("expired parent history not swept")
	}
}
