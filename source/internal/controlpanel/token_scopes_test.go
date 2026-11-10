package controlpanel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

var legacyScopesFixture = []string{"memory:read", "memory:write", "message:receive", "message:send", "session:manage", "session:read", "session:register", "session:rename", "session:spawn"}

// Produces old-format synthetic SQLite state, never a production database.
// Stripping metadata also keeps this fixture historical after scopes exist.
func oldScopeFixture(t *testing.T, version int) (string, []byte, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	seed, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if seed.Setup(testPassword) != nil || seed.CreateProject("demo") != nil {
		t.Fatal("fixture setup failed")
	}
	legacy, err := seed.IssueTokenFor("admin", "demo", "legacy", "admin")
	if err != nil {
		t.Fatal(err)
	}
	credentials := map[string]string{"legacy": legacy}
	if version == 3 {
		parent, err := seed.IssueProjectTokenFor("admin", "demo", "parent")
		if err != nil {
			t.Fatal(err)
		}
		credentials["project"] = parent
		req := DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", InstanceID: "client-scope-test", RequestID: "issued"}
		code, err := seed.BeginDeviceAuthorization("demo", parent, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = seed.ApproveDeviceAuthorization("admin", code.UserCode, "session", false); err != nil {
			t.Fatal(err)
		}
		grant, err := seed.ExchangeDeviceToken("demo", parent, code.DeviceCode, "asb-client-v1", false)
		if err != nil {
			t.Fatal(err)
		}
		credentials["session"] = grant.AccessToken
		req.RequestID, req.SessionID = "pending", "22222222-2222-4222-8222-222222222222"
		pending, err := seed.BeginDeviceAuthorization("demo", parent, req)
		if err != nil {
			t.Fatal(err)
		}
		credentials["pending-user"], credentials["pending-device"] = pending.UserCode, pending.DeviceCode
	}
	raw, err := json.Marshal(seed.data)
	if err != nil {
		t.Fatal(err)
	}
	if seed.Close() != nil {
		t.Fatal("fixture close failed")
	}
	var state map[string]any
	if json.Unmarshal(raw, &state) != nil {
		t.Fatal("fixture JSON failed")
	}
	state["version"] = version
	for _, v := range state["projects"].(map[string]any) {
		p := v.(map[string]any)
		delete(p, "projectGenerationId")
		for _, v := range p["tokens"].([]any) {
			token := v.(map[string]any)
			delete(token, "scopeVersion")
			delete(token, "scopes")
			delete(token, "admissionMode")
			delete(token, "issuedBy")
			delete(token, "maxAutoBindings")
		}
		if devices, ok := p["devices"].([]any); ok {
			for _, v := range devices {
				d := v.(map[string]any)
				delete(d, "scopeVersion")
				delete(d, "scopes")
			}
		}
		if version == 1 {
			delete(p, "members")
			for _, v := range p["tokens"].([]any) {
				token := v.(map[string]any)
				delete(token, "ownerID")
				delete(token, "issuedAt")
			}
		}
	}
	if version == 1 {
		delete(state, "users")
	}
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlitestore.Open(dir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	if db.Put("state", raw) != nil || db.Close() != nil {
		t.Fatal("fixture write failed")
	}
	return dir, raw, credentials
}

func assertLegacyScopeMetadata(t *testing.T, s *Store) {
	t.Helper()
	raw, err := json.Marshal(s.data)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Version  int `json:"version"`
		Projects map[string]struct {
			Tokens []struct {
				ScopeVersion int      `json:"scopeVersion"`
				Scopes       []string `json:"scopes"`
			} `json:"tokens"`
		} `json:"projects"`
	}
	if json.Unmarshal(raw, &st) != nil {
		t.Fatal("scope metadata decode failed")
	}
	for _, p := range st.Projects {
		for _, token := range p.Tokens {
			if token.ScopeVersion != 1 || !slices.Equal(token.Scopes, access.LegacyV1Scopes()) {
				t.Fatal("existing credential lacks the current LEGACY_V1 scopes")
			}
		}
	}
	if st.Version != scopeIntroductionVersion && st.Version != controlVersion {
		t.Fatal("control payload was not upgraded to the scope schema")
	}
}

func TestScopeMigrationKeepsExistingCredentials(t *testing.T) {
	dir, before, credentials := oldScopeFixture(t, 3)
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertLegacyScopeMetadata(t, s)
	// Reopening must not change any old credential or pending approval.
	var old state
	if json.Unmarshal(before, &old) != nil {
		t.Fatal("fixture state decode failed")
	}
	for i, token := range old.Projects["demo"].Tokens {
		var want, got map[string]any
		a, _ := json.Marshal(token)
		b, _ := json.Marshal(s.data.Projects["demo"].Tokens[i])
		_ = json.Unmarshal(a, &want)
		_ = json.Unmarshal(b, &got)
		delete(got, "scopeVersion")
		delete(got, "scopes")
		if !reflect.DeepEqual(want, got) {
			t.Fatal("migration changed existing credential fields")
		}
	}
	if !s.Authorize("demo", credentials["legacy"]) || !s.Authorize("demo", credentials["session"]) || s.Authorize("demo", credentials["project"]) {
		t.Fatal("existing MCP credential eligibility changed")
	}
	if pending, err := s.DeviceAuthorizationFor("admin", credentials["pending-user"]); err != nil || pending.Status != "pending" {
		t.Fatal("pending code lost")
	}
	if _, err := s.ApproveDeviceAuthorization("admin", credentials["pending-user"], "after-upgrade", false); err != nil {
		t.Fatal("pending approval no longer works")
	}
	if _, err := s.ExchangeDeviceToken("demo", credentials["project"], credentials["pending-device"], "asb-client-v1", false); err != nil {
		t.Fatal("same project credential cannot finish approval")
	}
	assertLegacyScopeMetadata(t, s)
	if s.Close() != nil {
		t.Fatal("close failed")
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertLegacyScopeMetadata(t, s)
	if !s.Authorize("demo", credentials["session"]) {
		t.Fatal("session credential lost across restart")
	}
}

func TestScopeMigrationLegacyVersions(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			dir, _, credentials := oldScopeFixture(t, version)
			s, err := OpenSQLite(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			assertLegacyScopeMetadata(t, s)
			if !s.Authorize("demo", credentials["legacy"]) || s.ProjectRole("admin", "demo") != "owner" {
				t.Fatal("legacy credential/account lost")
			}
		})
	}
}

func TestScopeMigrationWriteFailureRollsBack(t *testing.T) {
	dir, before, _ := oldScopeFixture(t, 3)
	s, err := openControlRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if json.Unmarshal(before, &s.data) != nil {
		t.Fatal("fixture decode failed")
	}
	memoryBefore, _ := json.Marshal(s.data)
	writes := 0
	s.write = func([]byte) error { writes++; return ErrStorage }
	if s.migrateScopes() == nil || writes != 1 {
		t.Fatal("migration ignored durable write failure")
	}
	after, _ := json.Marshal(s.data)
	if !bytes.Equal(memoryBefore, after) {
		t.Fatal("failed migration published partial metadata")
	}
	db, err := sqlitestore.Open(dir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	persisted, err := db.Get("state")
	if err != nil || !bytes.Equal(before, persisted) {
		t.Fatal("failed migration changed SQLite state")
	}
	s.write = func(raw []byte) error { return db.Put("state", raw) }
	if s.migrateScopes() != nil {
		t.Fatal("migration retry failed")
	}
	assertLegacyScopeMetadata(t, s)
}

func TestScopeV4RejectsInvalidMetadataWithoutWrites(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing-scopes":  func(token map[string]any) { delete(token, "scopes") },
		"null-scopes":     func(token map[string]any) { token["scopes"] = nil },
		"empty-scopes":    func(token map[string]any) { token["scopes"] = []string{} },
		"unknown-scope":   func(token map[string]any) { token["scopes"] = []string{"unknown:scope"} },
		"duplicate":       func(token map[string]any) { token["scopes"] = []string{"session:read", "session:read"} },
		"reordered":       func(token map[string]any) { token["scopes"] = []string{"session:register", "session:read"} },
		"missing-version": func(token map[string]any) { delete(token, "scopeVersion") },
		"future-version":  func(token map[string]any) { token["scopeVersion"] = 2 },
		"child-auto":      func(token map[string]any) { token["scopes"] = []string{"session:register:auto"} },
		"unknown-field":   func(token map[string]any) { token["unrecognizedScopeField"] = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _, _ := oldScopeFixture(t, 3)
			s, err := OpenSQLite(dir)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			db, err := sqlitestore.Open(dir, "control.db")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := db.Get("state")
			if err != nil {
				t.Fatal(err)
			}
			var st map[string]any
			if json.Unmarshal(raw, &st) != nil {
				t.Fatal("decode failed")
			}
			p := st["projects"].(map[string]any)["demo"].(map[string]any)
			mutate(p["tokens"].([]any)[0].(map[string]any))
			raw, _ = json.Marshal(st)
			if db.Put("state", raw) != nil || db.Close() != nil {
				t.Fatal("fixture mutation failed")
			}
			if invalid, err := OpenSQLite(dir); err == nil {
				invalid.Close()
				t.Fatal("v4 metadata silently defaulted")
			}
			db, err = sqlitestore.Open(dir, "control.db")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			after, err := db.Get("state")
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("rejected payload was rewritten")
			}
		})
	}
}

func TestScopeMigrationDoesNotReactivateHistory(t *testing.T) {
	dir, raw, _ := oldScopeFixture(t, 3)
	var st state
	if json.Unmarshal(raw, &st) != nil {
		t.Fatal("decode failed")
	}
	p := st.Projects["demo"]
	original := p.Tokens[2] // issued session grant
	credentials := map[string]string{}
	for i, kind := range []string{"revoked", "expired", "orphan"} {
		token := original
		value := "synthetic-inactive-" + kind
		token.TokenHash = digest(value)
		token.ID = strings.Repeat(string(rune('a'+i)), 52)
		token.Nickname = kind
		if kind == "revoked" {
			token.Revoked = true
			token.RevokedAt = time.Now().UTC()
		}
		if kind == "expired" {
			token.ExpiresAt = time.Now().Add(-time.Minute).UTC()
			token.ExpiredAt = time.Now().UTC()
		}
		if kind == "orphan" {
			token.ParentID = strings.Repeat("z", 52)
		}
		p.Tokens = append(p.Tokens, token)
		credentials[kind] = value
	}
	st.Projects["demo"] = p
	raw, _ = json.Marshal(st)
	db, err := sqlitestore.Open(dir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	if db.Put("state", raw) != nil || db.Close() != nil {
		t.Fatal("fixture write failed")
	}
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertLegacyScopeMetadata(t, s)
	if len(s.data.Projects["demo"].Tokens) != len(p.Tokens) {
		t.Fatal("migration removed history")
	}
	for kind, value := range credentials {
		if s.Authorize("demo", value) {
			t.Fatal("migration reactivated " + kind)
		}
	}
	for i, token := range p.Tokens {
		got := s.data.Projects["demo"].Tokens[i]
		if token.ID != got.ID || token.TokenHash != got.TokenHash || token.ParentID != got.ParentID || token.BoundIdentity != got.BoundIdentity || !token.ExpiresAt.Equal(got.ExpiresAt) || token.Revoked != got.Revoked {
			t.Fatal("migration changed inactive credential identity/state")
		}
	}
}

func TestScopeSnapshotsCannotMutateStore(t *testing.T) {
	dir, _, credentials := oldScopeFixture(t, 3)
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	view := s.Projects()[0]
	view.Tokens[0].Scopes[0] = "changed"
	g, _, ok := s.authorize("demo", credentials["session"])
	if !ok {
		t.Fatal("authorize failed")
	}
	g.Scopes[0] = "changed"
	assertLegacyScopeMetadata(t, s)
}

func TestScopeMigrationPayloadGrowthAndRestart(t *testing.T) {
	dir, raw, _ := oldScopeFixture(t, 3)
	var st map[string]any
	if json.Unmarshal(raw, &st) != nil {
		t.Fatal("fixture decode failed")
	}
	p := st["projects"].(map[string]any)["demo"].(map[string]any)
	template := p["tokens"].([]any)[0].(map[string]any)
	for i := 0; i < 1400; i++ {
		token := maps.Clone(template)
		token["id"] = fmt.Sprintf("%052d", 1000+i)
		token["tokenHash"] = digest(fmt.Sprintf("synthetic-large-%d", i))
		token["nickname"] = fmt.Sprintf("history-%d", i)
		token["boundIdentity"] = strings.Repeat("x", 1000)
		token["revoked"] = true
		token["revokedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		p["tokens"] = append(p["tokens"].([]any), token)
	}
	raw, _ = json.Marshal(st)
	for len(raw) > legacyControlMaxBytes {
		tokens := p["tokens"].([]any)
		p["tokens"] = tokens[:len(tokens)-10]
		raw, _ = json.Marshal(st)
	}
	db, err := sqlitestore.Open(dir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	if db.Put("state", raw) != nil || db.Close() != nil {
		t.Fatal("large fixture write failed")
	}
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal("large legacy DB could not upgrade")
	}
	assertLegacyScopeMetadata(t, s)
	enlarged, _ := json.Marshal(s.data)
	if len(enlarged) <= legacyControlMaxBytes || len(enlarged) > controlMaxBytes {
		t.Fatal("fixture did not exercise v4 payload growth")
	}
	if len(s.data.Projects["demo"].Tokens) != len(p["tokens"].([]any)) {
		t.Fatal("migration truncated old tokens")
	}
	s.Close()
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal("grown v4 DB could not restart")
	}
	defer s.Close()
	assertLegacyScopeMetadata(t, s)
	if _, err := parseControlState(append(raw, bytes.Repeat([]byte(" "), legacyControlMaxBytes-len(raw)+1)...)); err == nil {
		t.Fatal("legacy input size limit widened")
	}
}

func TestScopeV4ParentAndPendingBounds(t *testing.T) {
	dir, _, _ := oldScopeFixture(t, 3)
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw, _ := json.Marshal(s.data)
	var base state
	if json.Unmarshal(raw, &base) != nil {
		t.Fatal("state decode failed")
	}
	mutate := func(change func(*Project)) bool {
		var candidate state
		_ = json.Unmarshal(raw, &candidate)
		p := candidate.Projects["demo"]
		change(&p)
		candidate.Projects["demo"] = p
		return validState(candidate)
	}
	if mutate(func(p *Project) { p.Tokens[1].Scopes = []string{"session:register"} }) {
		t.Fatal("child exceeded parent scopes")
	}
	if mutate(func(p *Project) { p.Devices[0].Scopes = nil }) {
		t.Fatal("pending metadata missing accepted")
	}
	if mutate(func(p *Project) { p.Devices[0].Scopes = []string{"session:register:auto"} }) {
		t.Fatal("pending request delegated auto")
	}
	if mutate(func(p *Project) { p.Tokens[1].Scopes = []string{"memory:read"} }) {
		t.Fatal("project token lacks registration scope")
	}
	candidate := base
	candidate.Version = 3
	if validState(candidate) {
		t.Fatal("scoped metadata accepted as legacy-v3")
	}
}
