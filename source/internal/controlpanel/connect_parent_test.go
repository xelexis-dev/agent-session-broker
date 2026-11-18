package controlpanel

import (
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestConnectParentAuthorizationAndRevoke(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProject("connect"); err != nil {
		t.Fatal(err)
	}
	now := s.now().UTC()
	id := rand.Text() + rand.Text()
	raw := "asb_s_" + rand.Text() + rand.Text()
	parent := map[string]any{"v": 1, "id": id, "hash": digest("synthetic-parent"), "user": "admin", "rev": s.data.Users["admin"].Revision, "label": "test", "scopes": []string{"connect"}, "source": "panel", "created_at": now, "expires_at": now.Add(180 * 24 * time.Hour), "last_used_at": time.Time{}, "connect": map[string]any{"admission_mode": "manual", "auto_projects": []any{}, "mcp_scopes": access.LegacyV1Scopes(), "max_sessions": 32}}
	encoded, _ := json.Marshal(parent)
	setting := []byte(`{"v":1,"enabled":true}`)
	if err = s.writeBatch(map[string][]byte{"user-tokens/admin/" + id: encoded, "settings/connect": setting}, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.listRecords()
	s.loadAPI(rows)
	p := s.data.Projects["connect"]
	p.Tokens = append(p.Tokens, SessionToken{ScopeVersion: access.ScopeVersion, Scopes: access.LegacyV1Scopes(), Kind: tokenSession, ParentID: id, ID: rand.Text() + rand.Text(), OwnerID: "admin", Nickname: "synthetic", IssuedAt: now, ExpiresAt: now.Add(sessionLifetime), BoundIdentity: `["codex","synthetic","pc-example"]`, TokenHash: digest(raw)})
	if err = s.commitProject(p); err != nil {
		t.Fatal(err)
	}
	if !s.Authorize("connect", raw) {
		t.Fatal("connect parent session rejected: project-only parent lookup")
	}
	if err = s.RevokeOwnUserToken("admin", id); err != nil {
		t.Fatal(err)
	}
	if s.Authorize("connect", raw) {
		t.Fatal("revoked connect parent authorized")
	}
}
