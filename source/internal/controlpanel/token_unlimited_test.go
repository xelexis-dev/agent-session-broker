package controlpanel

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestAutoTokenUnlimitedFormRestartAndRegistrations(t *testing.T) {
	s := directStore(t)
	if s.CreateUser("admin", "owner", testPassword, "user") != nil || s.SetProjectMember("admin", "demo", "owner", "owner") != nil {
		t.Fatal("owner fixture failed")
	}
	s.data.ScopeIssuanceReady = true
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	w := owner.call("POST", "/tokens/project", url.Values{"project": {"demo"}, "label": {"unlimited-fixture"}, "scope_selection": {"custom"}, "scope": {access.SessionRegister, access.SessionRegisterAuto}, "auto_cap": {"unlimited"}, "auto_days": {"unlimited"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("explicit unlimited issuance status=%d, want 303", w.Code)
	}
	raw := issuedTokenFrom(t, owner.call("GET", w.Header().Get("Location"), nil))
	dir := s.root.Name()
	if err := s.Close(); err != nil {
		t.Fatal("fixture close failed")
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal("unlimited state failed strict reload")
	}
	defer reopened.Close()
	reopened.now = func() time.Time { return now }
	token := reopened.ProjectsFor("owner")[0].Tokens[0]
	if token.MaxAutoBindings != 0 || !token.ExpiresAt.IsZero() {
		t.Fatal("unlimited metadata changed on restart")
	}
	for i := 0; i < 65; i++ {
		r := directRequest(directForm())
		r.SessionID = fmt.Sprintf("%08d-1111-4111-8111-111111111111", i+1)
		r.RequestID = fmt.Sprintf("unlimited-%d", i)
		r.RequestedNickname = fmt.Sprintf("worker-%d", i)
		out, err := reopened.ExchangeDirect("demo", raw, r, DirectProof{}, "fixture-peer", false)
		if err != nil {
			t.Fatalf("registration %d failed", i+1)
		}
		if out.ExpiresIn <= 0 {
			t.Fatal("unlimited parent gave expired child")
		}
		b := reopened.data.Projects["demo"].AutoBindings[i]
		id := fmt.Sprintf("registration-%d", i)
		if err := reopened.directRegistration("demo", out.GrantID, b.Identity, id, "", out.Nickname, 0, false); err != nil {
			t.Fatal("claim failed")
		}
		if err := reopened.directRegistration("demo", out.GrantID, b.Identity, id, "owner", out.Nickname, 1, true); err != nil {
			t.Fatal("confirm failed")
		}
		now = now.Add(10 * time.Second)
	}
	if len(reopened.data.Projects["demo"].AutoBindings) != 65 {
		t.Fatal("unlimited parent did not retain 65 bindings")
	}
	now = now.Add(100 * 365 * 24 * time.Hour)
	if err := reopened.SweepExpired(); err != nil {
		t.Fatal("sweep failed")
	}
	if _, _, err := reopened.directParentLocked("demo", raw); err != nil {
		t.Fatal("unlimited parent expired or history was swept")
	}
}

func TestAutoTokenOffLongNoticeAbsent(t *testing.T) {
	s := userStore(t)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	body := owner.call("GET", "/projects/settings?project=demo", nil).Body.String()
	if strings.Contains(body, "인스턴스 설정에서 제한 권한·자동 등록 토큰 발급이 꺼져 있습니다") {
		t.Fatal("old long gate-off notice remains")
	}
	if !strings.Contains(body, `class="token-scope-options" disabled`) {
		t.Fatal("removing notice enabled restricted issuance")
	}
}

func TestAutoTokenUnlimitedRevocationAndAudit(t *testing.T) {
	s := directStore(t)
	raw, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "unlimited", ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto), UnlimitedAutoBindings: true, UnlimitedAutoLifetime: true})
	if err != nil {
		t.Fatal("issuance failed")
	}
	out, err := s.ExchangeDirect("demo", raw, directRequest(directForm()), DirectProof{}, "fixture-peer", false)
	if err != nil {
		t.Fatal("exchange failed")
	}
	events, err := s.DirectAuditFor("admin", "demo")
	if err != nil || len(events) < 2 {
		t.Fatal("unlimited registration missing audit")
	}
	id := s.data.Projects["demo"].Tokens[0].ID
	if err := s.RevokeTokenFor("admin", "demo", id); err != nil {
		t.Fatal("parent revocation failed")
	}
	if _, _, ok := s.authorize("demo", out.AccessToken); ok {
		t.Fatal("unlimited parent revocation did not cascade")
	}
}
