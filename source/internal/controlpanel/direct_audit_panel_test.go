package controlpanel

import (
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newDirectAuditPanelFixture(t *testing.T) (*Store, http.Handler) {
	t.Helper()
	s := directStore(t)
	// Real exchange and policy confirmation; synthetic identities and credentials.
	for _, id := range []string{"owner", "viewer", "outsider"} {
		if err := s.CreateUser("admin", id, testPassword, "user"); err != nil {
			t.Fatal(err)
		}
	}
	for id, role := range map[string]string{"owner": "owner", "viewer": "viewer"} {
		if err := s.SetProjectMember("admin", "demo", id, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateUser("admin", "developer", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectMember("admin", "demo", "developer", "developer"); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueProjectTokenWithOptionsFor("owner", "demo", "worker-parent", ProjectTokenOptions{Scopes: []string{access.SessionRegister, access.SessionRegisterAuto}})
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	if w := directPOST(t, h, raw, directForm()); w.Code != 200 {
		t.Fatal("direct exchange failed")
	}
	// Confirmation is the same store callback invoked after broker registration.
	p := s.data.Projects["demo"]
	child := p.AutoBindings[0]
	if err := s.directRegistration("demo", child.GrantID, child.Identity, "fixture-registration", "", "worker", 0, false); err != nil {
		t.Fatal(err)
	}
	if err := s.directRegistration("demo", child.GrantID, child.Identity, "fixture-registration", "owner", "worker", 1, true); err != nil {
		t.Fatal(err)
	}
	bad := directForm()
	bad.Set("request_id", "denied-request")
	bad.Set("requested_nickname", "broker-admin")
	if w := directPOST(t, h, raw, bad); w.Code != 409 {
		t.Fatal("expected an identity conflict denial")
	}
	return s, h
}

func TestDirectAuditPageListsRegistrationAndDenial(t *testing.T) {
	_, h := newDirectAuditPanelFixture(t)
	owner := loginPanelUser(t, h, "owner")
	page := owner.call("GET", "/projects/audit?project=demo", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `data-audit-kind="registration"`) || !strings.Contains(page.Body.String(), `data-audit-kind="rejected"`) {
		t.Fatal("audit page lacks registration and denial events")
	}
	dev := loginPanelUser(t, h, "developer")
	hidden := dev.call("GET", "/projects/audit?project=demo", nil)
	if hidden.Code != 200 || strings.Contains(hidden.Body.String(), `data-audit-kind=`) {
		t.Fatal("foreign parent audit exposed")
	}
}

func TestDirectAuditRolesGateAndReadOnly(t *testing.T) {
	s, h := newDirectAuditPanelFixture(t)
	for _, tc := range []struct {
		id     string
		status int
		events bool
	}{{"owner", 200, true}, {"admin", 200, true}, {"developer", 200, false}, {"viewer", 403, false}, {"outsider", 403, false}} {
		client := loginPanelUser(t, h, tc.id)
		response := client.call("GET", "/projects/audit?project=demo", nil)
		if response.Code != tc.status || strings.Contains(response.Body.String(), `data-audit-kind=`) != tc.events {
			t.Fatalf("audit role %s: unexpected status/events", tc.id)
		}
	}
	dev := loginPanelUser(t, h, "developer")
	s.mu.Lock()
	var own AuditEvent
	for _, event := range s.audits {
		own = event
		break
	}
	own.OwnerID = "developer"
	own.Kind = "expired"
	own.Reason = "parent-inactive"
	own.GrantID = ""
	own.ParentID = strings.Repeat("d", 52)
	s.audits["fixture-own"] = own
	s.mu.Unlock()
	response := dev.call("GET", "/projects/audit?project=demo", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `data-audit-kind="expired"`) || strings.Contains(response.Body.String(), `data-audit-kind="registration"`) {
		t.Fatal("developer own-only filter failed")
	}
	if err := s.SetProjectMember("admin", "demo", "developer", ""); err != nil {
		t.Fatal(err)
	}
	if got := dev.call("GET", "/projects/audit?project=demo", nil); got.Code != 403 {
		t.Fatal("removed member retained audit")
	}
	owner := loginPanelUser(t, h, "owner")
	writes := 0
	original := s.writeBatch
	s.writeBatch = func(puts map[string][]byte, deletes []string) error { writes++; return original(puts, deletes) }
	visible := owner.call("GET", "/projects/audit?project=demo", nil)
	if visible.Code != 200 || writes != 0 {
		t.Fatal("read-only audit wrote storage")
	}
	for _, secret := range []string{own.Binding, own.ParentID, own.GrantID} {
		if secret != "" && strings.Contains(visible.Body.String(), secret) {
			t.Fatal("internal audit reference exposed")
		}
	}
	if strings.Contains(visible.Body.String(), "asb_"+"p_") || strings.Contains(visible.Body.String(), "asb_"+"s_") {
		t.Fatal("credential prefix exposed")
	}
	if !strings.Contains(visible.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("audit cached")
	}
	s.mu.Lock()
	s.data.ScopeIssuanceReady = false
	s.mu.Unlock()
	disabled := owner.call("GET", "/projects/audit?project=demo", nil)
	if disabled.Code != 200 || !strings.Contains(disabled.Body.String(), "자동 등록이 활성화되지 않았습니다.") || strings.Contains(disabled.Body.String(), `data-audit-kind=`) || strings.Contains(disabled.Body.String(), "worker-parent") {
		t.Fatal("gate off rendered audit data")
	}
	if writes != 0 {
		t.Fatal("gate off read wrote storage")
	}
	if result := owner.call("POST", "/projects/audit?project=demo", url.Values{}); result.Code != 405 {
		t.Fatal("audit accepted POST")
	}
	for _, query := range []string{"", "?project=demo&project=other", "?project=demo&extra=x"} {
		if out := owner.call("GET", "/projects/audit"+query, nil); out.Code != 400 {
			t.Fatal("invalid audit query accepted")
		}
	}
	if safeLoginNext("/projects/audit?project=demo") == "" {
		t.Fatal("audit cannot return after login")
	}
}

func TestDirectAuditCapProjectionAndCapture(t *testing.T) {
	s, h := newDirectAuditPanelFixture(t)
	owner := loginPanelUser(t, h, "owner")
	s.mu.Lock()
	var sample AuditEvent
	for _, event := range s.audits {
		if event.Kind == "registration" {
			sample = event
			break
		}
	}
	s.audits = map[string]AuditEvent{}
	kinds := []string{"request", "grant", "registration", "rejected", "revoked", "expired"}
	for i := 0; i < 105; i++ {
		e := sample
		e.Kind = kinds[i%len(kinds)]
		e.Reason = map[string]string{"request": "accepted", "grant": "issued", "registration": "confirmed", "rejected": "conflict", "revoked": "explicit-release", "expired": "parent-inactive"}[e.Kind]
		e.At = time.Now().UTC().Add(-time.Duration(i) * time.Second)
		s.audits[fmt.Sprintf("fixture-%d", i)] = e
	}
	s.mu.Unlock()
	page := owner.call("GET", "/projects/audit?project=demo", nil)
	body := page.Body.String()
	if page.Code != 200 || strings.Count(body, `data-audit-kind=`) != 100 || !strings.Contains(body, "최근 100건만 표시") {
		t.Fatal("audit cap failed")
	}
	for _, kind := range kinds {
		if !strings.Contains(body, `data-audit-kind="`+kind+`"`) {
			t.Fatal("event kind missing")
		}
	}
	for _, value := range []string{"worker-parent", "worker", "fixture-registration", "#session-registration:fixture-registration", "datetime=", "data-local-time", "저장 안 됨"} {
		if !strings.Contains(body, value) {
			t.Fatalf("projection missing %s", value)
		}
	}
	settings := owner.call("GET", "/projects/settings?project=demo", nil)
	if !strings.Contains(settings.Body.String(), `href="/projects/audit?project=demo"`) {
		t.Fatal("settings audit link missing")
	}
	if dir := os.Getenv("ASB_AUDIT_CAPTURE_DIR"); dir != "" {
		s.mu.Lock()
		for key := range s.audits {
			keep := false
			for i := 0; i < 6; i++ {
				if key == fmt.Sprintf("fixture-%d", i) {
					keep = true
				}
			}
			if !keep {
				delete(s.audits, key)
			}
		}
		s.mu.Unlock()
		page = owner.call("GET", "/projects/audit?project=demo", nil)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "audit.html"), page.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
