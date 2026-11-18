package controlpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestConnectReviewAPIBearerApproval(t *testing.T) {
	s := connectFixture(t)
	raw, _ := connectIssue(t, s, "manual", 0)
	d := connectPending(t, s, "demo", raw, "api-self-approval")
	if err := s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	apiRaw, _, err := s.IssueOwnUserToken("admin", s.data.Users["admin"].Revision, "approve", 3600, []string{"approve"}, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	body, _ := json.Marshal(map[string]string{"user_code": d.UserCode, "action": "approve", "nickname": "api-self-approval"})
	r := httptest.NewRequest("POST", "https://broker/api/v1/device-approvals/decision", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+apiRaw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("API bearer approved connect request: status=%d", w.Code)
	}
	owner := loginPanelUser(t, h, "admin")
	approved := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {d.UserCode}, "nickname": {"panel-approved"}})
	if approved.Code != 303 {
		t.Fatalf("panel approval status=%d", approved.Code)
	}
}

func connectReviewManagementFixture(t *testing.T) (*Store, access.Grant, string) {
	t.Helper()
	s := connectFixture(t)
	if err := s.CreateUser("admin", "owner", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectMember("admin", "demo", "owner", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectMember("admin", "demo", "admin", "developer"); err != nil {
		t.Fatal(err)
	}
	raw, _ := connectIssue(t, s, "manual", 0)
	token := connectApproved(t, s, "demo", raw, "caller")
	actor, _, ok := s.authorize("demo", token.AccessToken)
	if !ok {
		t.Fatal("caller grant")
	}
	other, err := s.IssueTokenFor("owner", "demo", "target", "owner")
	if err != nil {
		t.Fatal(err)
	}
	target, _, ok := s.authorize("demo", other)
	if !ok {
		t.Fatal("target grant")
	}
	identity := `["codex","target","pc-example"]`
	if target.Claim(identity) != nil || target.Connected(identity) != nil {
		t.Fatal("target binding")
	}
	return s, actor, identity
}
func TestConnectReviewExplicitManagementRole(t *testing.T) {
	for _, action := range []string{"manage", "retire-registration", "retire-name"} {
		t.Run(action, func(t *testing.T) {
			_, actor, identity := connectReviewManagementFixture(t)
			var err error
			switch action {
			case "manage":
				err = actor.Manage(identity, "owner")
			case "retire-registration":
				_, err = actor.Retire(access.RetirementTarget{Identity: identity, RecordedOwner: "owner", RegistrationID: "synthetic-registration", Name: "target"}, nil)
			case "retire-name":
				_, err = actor.Retire(access.RetirementTarget{Name: "target"}, nil)
			}
			if err == nil {
				t.Fatal("explicit Developer used admin implicit Owner to manage another member")
			}
		})
	}
}

func TestConnectReviewAuditIsolation(t *testing.T) {
	for _, parentless := range []bool{false, true} {
		t.Run(map[bool]string{false: "parent", true: "parentless-user"}[parentless], func(t *testing.T) {
			s := connectFixture(t)
			if err := s.CreateUser("admin", "otheruser", testPassword, "user"); err != nil {
				t.Fatal(err)
			}
			if err := s.SetProjectMember("admin", "demo", "otheruser", "developer"); err != nil {
				t.Fatal(err)
			}
			_, other, err := s.issueConnectToken("otheruser", 0, connectOptions{})
			if err != nil {
				t.Fatal(err)
			}
			raw, parent := connectIssue(t, s, "manual", 0)
			token := connectApproved(t, s, "demo", raw, "flood-child")
			foreign := connectEvent(other, Project{}, "", "token-issued", "explicit", "otheruser")
			flood := connectEvent(parent, Project{}, "", "token-issued", "explicit", "admin")
			if parentless {
				foreign = connectAudit{V: 1, Kind: "switch-changed", Reason: "enabled", ActorKind: "user", ActorID: "otheruser"}
				flood = connectAudit{V: 1, Kind: "switch-changed", Reason: "enabled", ActorKind: "user", ActorID: "admin"}
			}
			s.mu.Lock()
			err = s.commitConnect(s.data, nil, nil, nil, nil, []connectAudit{foreign}, nil)
			s.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			events := make([]connectAudit, 5000)
			for i := range events {
				events[i] = flood
			}
			s.mu.Lock()
			err = s.commitConnect(s.data, nil, nil, nil, nil, events, nil)
			s.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			survived := false
			count := 0
			for _, e := range s.connect.audits {
				if !parentless && e.ParentID == other.ID || parentless && e.ParentID == "" && e.ActorID == "otheruser" {
					survived = true
				}
				if !parentless && e.ParentID == parent.ID || parentless && e.ParentID == "" && e.ActorID == "admin" {
					count++
				}
			}
			if !survived {
				t.Error("A flood evicted B audit")
			}
			limit := 256
			if parentless {
				limit = 512
			}
			if count > limit {
				t.Errorf("audit bucket count=%d exceeds %d", count, limit)
			}
			if err = s.RevokeOwnUserToken("admin", parent.ID); err != nil {
				t.Fatal(err)
			}
			rows, _ := s.listRecords()
			parentRevoke, childRevoke := false, false
			for key, raw := range rows {
				if !strings.HasPrefix(key, "connect-audit/") {
					continue
				}
				var e connectAudit
				if json.Unmarshal(raw, &e) != nil {
					t.Fatal("audit decode")
				}
				if e.ParentID != parent.ID {
					continue
				}
				if e.Kind == "token-revoked" {
					parentRevoke = true
				}
				if e.Kind == "grant-revoked" && e.GrantID == token.GrantID {
					childRevoke = true
				}
			}
			if !parentRevoke || !childRevoke {
				t.Fatal("new revoke audit missing after saturation")
			}
		})
	}
}

func TestConnectReviewIssuanceSwitchOFF(t *testing.T) {
	s := connectFixture(t)
	_, existing := connectIssue(t, s, "manual", 0)
	if err := s.setConnectPolicy("admin", false); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.issueConnectToken("admin", 0, connectOptions{})
	if !errors.Is(err, errConnectDisabled) {
		t.Fatal("switch OFF allowed issuance")
	}
	if _, err = s.ListOwnUserTokens("admin"); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeOwnUserToken("admin", existing.ID); err != nil {
		t.Fatal("OFF blocked revoke", err)
	}
}

func TestConnectReviewFirstChildExpiryAudit(t *testing.T) {
	s := connectFixture(t)
	raw, parent := connectIssue(t, s, "manual", 0)
	token := connectApproved(t, s, "demo", raw, "idle-child")
	if !s.Authorize("demo", token.AccessToken) {
		t.Fatal("fixture authorize")
	}
	now := s.now().UTC()
	future := now.Add(sessionIdleLifetime + time.Second)
	s.now = func() time.Time { return future }
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		n := 0
		rows, _ := s.listRecords()
		for key, raw := range rows {
			if !strings.HasPrefix(key, "connect-audit/") {
				continue
			}
			var e connectAudit
			if json.Unmarshal(raw, &e) == nil && e.ParentID == parent.ID && e.GrantID == token.GrantID && e.Kind == "grant-revoked" && e.Reason == "expired" {
				n++
			}
		}
		return n
	}
	if n := count(); n != 1 {
		t.Errorf("first child expiry audit count=%d want1", n)
	}
	if _, ok := s.api.tokens[parent.ID]; !ok {
		t.Fatal("live parent expired")
	}
	future = future.Add(tokenHistoryRetention + time.Second)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("history removal duplicated/lost expiry audit count=%d", n)
	}
}

func TestConnectReviewOtherOwnerApproval(t *testing.T) {
	s := connectFixture(t)
	for _, user := range []string{"owner", "member"} {
		if err := s.CreateUser("admin", user, testPassword, "user"); err != nil {
			t.Fatal(err)
		}
		role := "developer"
		if user == "owner" {
			role = "owner"
		}
		if err := s.SetProjectMember("admin", "demo", user, role); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := connectIssue(t, s, "manual", 0)
	d := connectPending(t, s, "demo", raw, "different-approver")
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	member := loginPanelUser(t, h, "member")
	refused := member.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {d.UserCode}, "nickname": {"member-tried"}})
	if refused.Code != 404 {
		t.Fatalf("plain member approval status=%d", refused.Code)
	}
	owner := loginPanelUser(t, h, "owner")
	approved := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {d.UserCode}, "nickname": {"owner-approved"}})
	if approved.Code != 303 {
		t.Fatalf("different Owner approval status=%d", approved.Code)
	}
}

func TestConnectReviewAuditUserIsolationAcrossParents(t *testing.T) {
	s := connectFixture(t)
	if err := s.CreateUser("admin", "otheruser", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	_, other, err := s.issueConnectToken("otheruser", 0, connectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	foreignKey := ""
	for key, e := range s.connect.audits {
		if e.ParentID == other.ID {
			foreignKey = key
			break
		}
	}
	if foreignKey == "" {
		t.Fatal("foreign row missing")
	}
	events := []connectAudit{}
	parents := map[string]bool{}
	for i := 0; i < 17; i++ {
		_, parent := connectIssue(t, s, "manual", 0)
		parents[parent.ID] = true
		for n := 0; n < 256; n++ {
			events = append(events, connectEvent(parent, Project{}, "", "token-issued", "explicit", "admin"))
		}
	}
	s.mu.Lock()
	err = s.commitConnect(s.data, nil, nil, nil, nil, events, nil)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := s.listRecords()
	if _, ok := rows[foreignKey]; !ok {
		t.Error("multi-parent A flood removed B oldest row")
	}
	count := 0
	byParent := map[string]int{}
	for _, e := range s.connect.audits {
		if e.UserID == "admin" || e.UserID == "" && e.ActorID == "admin" {
			count++
		}
		if e.ParentID != "" {
			byParent[e.ParentID]++
		}
	}
	if count > 512 {
		t.Errorf("user A audit count=%d exceeds 512", count)
	}
	for _, n := range byParent {
		if n > 256 {
			t.Error("parent quota exceeded")
		}
	}
	if err = s.RevokeOwnUserTokens("admin"); err != nil {
		t.Fatal("multi-parent revoke refused", err)
	}
	rows, _ = s.listRecords()
	revoked := map[string]bool{}
	for key, raw := range rows {
		if !strings.HasPrefix(key, "connect-audit/") {
			continue
		}
		var e connectAudit
		if json.Unmarshal(raw, &e) == nil && e.Kind == "token-revoked" && parents[e.ParentID] {
			revoked[e.ParentID] = true
		}
	}
	if len(revoked) != len(parents) {
		t.Fatal("new revoke rows evicted")
	}
	if _, ok := rows[foreignKey]; !ok {
		t.Fatal("revoke evicted B oldest row")
	}
}

func TestConnectReviewNilGuardIsNotPanel(t *testing.T) {
	s := connectFixture(t)
	raw, _ := connectIssue(t, s, "manual", 0)
	d := connectPending(t, s, "demo", raw, "nil-guard")
	p := &panel{store: s}
	if _, err := p.decideDeviceCode("admin", d.UserCode, "approve", "nil-guard", nil, false); !errors.Is(err, ErrForbidden) {
		t.Fatal("nil guard inferred panel authority")
	}
}
