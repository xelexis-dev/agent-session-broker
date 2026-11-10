package controlpanel

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func spawnFixture(t *testing.T, empty string) (string, state, map[string][]byte) {
	t.Helper()
	dir, _, _ := oldScopeFixture(t, 3)
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.SetScopeIssuanceReady("admin", true) != nil {
		t.Fatal("fixture scope issuance")
	}
	autoParent := directParent(t, s, 8)
	if response := directPOST(t, New(s, tokenViewBackend{}, http.NotFoundHandler()), autoParent, directForm()); response.Code != http.StatusOK {
		t.Fatal("fixture auto grant")
	}
	p := s.data.Projects["demo"]
	for i := range p.Tokens {
		auto := access.HasScope(p.Tokens[i].Scopes, access.SessionRegisterAuto)
		p.Tokens[i].Scopes = slices.Clone(legacyScopesFixture)
		if auto {
			p.Tokens[i].Scopes = append(p.Tokens[i].Scopes, access.SessionRegisterAuto)
			slices.Sort(p.Tokens[i].Scopes)
		}
	}
	for i := range p.Devices {
		p.Devices[i].Scopes = slices.Clone(legacyScopesFixture)
	}
	var grant SessionToken
	for _, token := range p.Tokens {
		if token.Kind == tokenSession {
			grant = token
			break
		}
	}
	for i, kind := range []string{"revoked", "expired", "orphan"} {
		token := grant
		token.ID = strings.Repeat(string(rune('a'+i)), 52)
		token.TokenHash = digest("synthetic-history-" + kind)
		token.Nickname = kind
		switch kind {
		case "revoked":
			token.Revoked, token.RevokedAt = true, time.Now().UTC()
		case "expired":
			token.ExpiresAt, token.ExpiredAt = time.Now().Add(-time.Minute).UTC(), time.Now().UTC()
		case "orphan":
			token.ParentID = strings.Repeat("z", 52)
		}
		p.Tokens = append(p.Tokens, token)
	}
	deviceBase := p.Devices[len(p.Devices)-1]
	for _, kind := range []string{"denied", "expired", "orphan"} {
		device := deviceBase
		device.CodeHash, device.UserHash = digest("synthetic-device-"+kind), digest("synthetic-user-"+kind)
		device.Request.RequestID = "historical-" + kind
		switch kind {
		case "denied":
			device.Status = "denied"
		case "expired":
			device.CreatedAt, device.ExpiresAt = time.Now().Add(-2*time.Minute).UTC(), time.Now().Add(-time.Minute).UTC()
		case "orphan":
			device.TokenID = strings.Repeat("z", 52)
		}
		p.Devices = append(p.Devices, device)
	}
	for i := range p.Tokens {
		if empty == "grant" && p.Tokens[i].Kind == tokenSession {
			p.Tokens[i].Scopes = []string{access.SessionSpawn}
			break
		}
	}
	if empty == "device" {
		p.Devices[0].Scopes = []string{access.SessionSpawn}
	}
	s.data.Projects["demo"] = p
	var parent SessionToken
	for _, tok := range p.Tokens {
		if tok.Kind == tokenProject {
			parent = tok
			break
		}
	}
	event := AuditEvent{Version: 1, Project: "demo", ProjectGenerationID: p.ProjectGenerationID, ParentID: parent.ID, OwnerID: parent.OwnerID, Kind: tokenScopesAuditKind, At: time.Now().UTC(), TokenScopes: &TokenScopeAudit{ActorID: "admin", Label: "historical", Before: slices.Clone(legacyScopesFixture), After: []string{access.SessionRegister, access.SessionSpawn}}}
	if s.commitAudited(s.data, []AuditEvent{event}) != nil {
		t.Fatal("fixture commit")
	}
	before := s.data
	if s.Close() != nil {
		t.Fatal("close fixture")
	}
	db, err := sqlitestore.Open(dir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	records, err := db.List()
	if err != nil {
		t.Fatal(err)
	}
	if db.Close() != nil {
		t.Fatal("close db")
	}
	return dir, before, records
}

func TestSpawnMigrationV5(t *testing.T) {
	dir, want, records := spawnFixture(t, "")
	// Compare the entire payload: scope version, admission and terminal state stay identical.
	for name, p := range want.Projects {
		for i := range p.Tokens {
			p.Tokens[i].Scopes = slices.DeleteFunc(slices.Clone(p.Tokens[i].Scopes), func(v string) bool { return v == access.SessionSpawn })
		}
		for i := range p.Devices {
			p.Devices[i].Scopes = slices.DeleteFunc(slices.Clone(p.Devices[i].Scopes), func(v string) bool { return v == access.SessionSpawn })
		}
		want.Projects[name] = p
	}
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(s.data)
	expected, _ := json.Marshal(want)
	if !bytes.Equal(actual, expected) {
		t.Error("migration must change only active spawn scopes")
	}
	if s.data.Version != 5 || !validScopeState(s.data) {
		t.Error("version/subset invariant")
	}
	if s.Close() != nil {
		t.Fatal("close")
	}
	db, err := sqlitestore.Open(dir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	after, err := db.List()
	if err != nil {
		t.Fatal(err)
	}
	for key, raw := range records {
		if key != "state" && !bytes.Equal(raw, after[key]) {
			t.Error("audit history changed")
		}
	}
	db.Close()
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	s.write = func([]byte) error { writes++; return ErrStorage }
	if s.migrateControlState() != nil || writes != 0 {
		t.Error("reopen migration must not commit")
	}
	s.Close()
}

func TestSpawnMigrationEmptyFailsWithoutWrite(t *testing.T) {
	for _, kind := range []string{"grant", "device"} {
		t.Run(kind, func(t *testing.T) {
			dir, _, _ := spawnFixture(t, kind)
			path := filepath.Join(dir, "control.db")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			s, err := OpenSQLite(dir)
			if s != nil {
				s.Close()
			}
			if !errors.Is(err, ErrStorage) {
				t.Error("spawn-only scope list must refuse startup")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if sha256.Sum256(before) != sha256.Sum256(after) {
				t.Error("failed migration changed database")
			}
		})
	}
}

func TestSpawnMigrationWriteFailureAtomic(t *testing.T) {
	dir, before, records := spawnFixture(t, "")
	s, err := openControlRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.data = before
	original, _ := json.Marshal(s.data)
	writes := 0
	s.write = func([]byte) error { writes++; return ErrStorage }
	if !errors.Is(s.migrateSpawnScopes(), ErrStorage) || writes != 1 {
		t.Fatal("durable failure must abort migration")
	}
	after, _ := json.Marshal(s.data)
	if !bytes.Equal(original, after) {
		t.Fatal("failed commit published in-memory scopes")
	}
	db, err := sqlitestore.Open(dir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	persisted, err := db.Get("state")
	if err != nil || !bytes.Equal(persisted, records["state"]) {
		t.Fatal("failed commit changed stored scopes")
	}
	s.write = func(raw []byte) error { return db.Put("state", raw) }
	if s.migrateSpawnScopes() != nil {
		t.Fatal("retry failed")
	}
}

func TestSpawnIssuanceAndEditingRejected(t *testing.T) {
	s := userStore(t)
	if s.SetScopeIssuanceReady("admin", true) != nil {
		t.Fatal("enable issuance")
	}
	historical := []string{access.SessionRegister, access.SessionSpawn}
	if _, err := s.IssueProjectTokenWithOptionsFor("owner", "demo", "retired", ProjectTokenOptions{Scopes: historical}); !errors.Is(err, ErrInvalid) {
		t.Fatal("retired scope issuance accepted")
	}
	raw, err := s.IssueProjectTokenFor("owner", "demo", "current")
	if err != nil {
		t.Fatal(err)
	}
	parent := scopeEditParent(t, s, raw)
	if _, err := s.EditTokenScopesFor("owner", "demo", parent.ID, historical, []string{scopeEditVersion(s, parent)}); !errors.Is(err, ErrInvalid) {
		t.Fatal("retired scope edit accepted")
	}
}
