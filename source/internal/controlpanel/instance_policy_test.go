package controlpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func TestTokenOnlyNewProjectsAndLegacyAPIs(t *testing.T) {
	s := userStore(t)
	if err := s.CreateProject("created"); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportProjects([]string{"imported"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Projects() {
		if p.Mode != "token" || s.Authorize(p.Name, "") {
			t.Errorf("project %s permits unauthenticated access", p.Name)
		}
	}
	if s.AllowsUnauthenticated() {
		t.Error("instance allows unauthenticated access")
	}
	if err := s.SetAllowsUnauthenticated("admin", true); !errors.Is(err, ErrForbidden) {
		t.Errorf("legacy enable = %v", err)
	}
	for _, save := range []func() error{
		func() error { return s.SaveProject("created", "open", true, "") },
		func() error { return s.SaveProjectFor("admin", "created", "open", true) },
	} {
		if save() == nil {
			t.Error("legacy project save accepted open mode")
		}
	}
	// Defense in depth: even an old in-memory flag/mode cannot authorize a request.
	s.mu.Lock()
	p := s.data.Projects["created"]
	p.Mode = "open"
	s.data.Projects["created"] = p
	s.data.DisableUnauthenticated = false
	s.mu.Unlock()
	if s.Authorize("created", "") {
		t.Error("legacy fields bypassed authorization")
	}
}

func TestTokenOnlyUIAndOldHTTPAPIs(t *testing.T) {
	s := userStore(t)
	h := New(s, &ownedPanelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	for _, path := range []string{"/", "/projects/settings?project=demo", "/projects/sessions?project=demo"} {
		page := admin.call("GET", path, nil)
		if page.Code != 200 {
			t.Fatalf("GET %s = %d", path, page.Code)
		}
		// The admin sidebar links to instance settings; its forms stay on that page.
		for _, removed := range []string{`name="mode"`, `value="open"`, `allowUnauthenticated`, `action="/admin/`, "무인증"} {
			if strings.Contains(page.Body.String(), removed) {
				t.Errorf("%s retains %s", path, removed)
			}
		}
	}
	if w := admin.call("GET", "/admin/settings", nil); w.Code != 200 || !strings.Contains(w.Body.String(), en("admin.http.allow_aria")) {
		t.Errorf("instance settings page = %d", w.Code)
	}
	for _, allowed := range []string{"true", "false"} {
		w := admin.call("POST", "/admin/settings/save", url.Values{"allowUnauthenticated": {allowed}})
		if w.Code != http.StatusGone {
			t.Errorf("removed setting API = %d", w.Code)
		}
	}
	if w := admin.call("POST", "/projects/save", url.Values{"name": {"demo"}, "mode": {"open"}, "enabled": {"true"}}); w.Code != 400 {
		t.Errorf("legacy open save = %d", w.Code)
	}
	if w := admin.call("POST", "/projects/save", url.Values{"name": {"demo"}, "enabled": {"true"}}); w.Code != 303 {
		t.Errorf("fixed token save = %d", w.Code)
	}
	if w := admin.call("POST", "/projects/create", url.Values{"name": {"forged"}, "mode": {"open"}}); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if s.Authorize("forged", "") {
		t.Error("creation form bypassed token policy")
	}
	admin.csrf = "wrong"
	if w := admin.call("POST", "/projects/save", url.Values{"name": {"demo"}, "enabled": {"true"}}); w.Code != 403 {
		t.Errorf("CSRF = %d", w.Code)
	}
}

func TestTokenOnlyLegacyMigrationPreservesState(t *testing.T) {
	for _, storage := range []string{"json-import", "sqlite"} {
		for _, disabled := range []bool{false, true} {
			t.Run(storage+"/disabled="+map[bool]string{false: "false", true: "true"}[disabled], func(t *testing.T) {
				dir := t.TempDir()
				seed, err := OpenSQLite(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { seed.Close() })
				if err := seed.Setup(testPassword); err != nil {
					t.Fatal(err)
				}
				if err := seed.CreateUser("admin", "owner", testPassword, "user"); err != nil {
					t.Fatal(err)
				}
				if err := seed.CreateProjectFor("owner", "legacy"); err != nil {
					t.Fatal(err)
				}
				if err := seed.SaveProjectFor("owner", "legacy", "token", true); err != nil {
					t.Fatal(err)
				}
				raw, err := seed.IssueTokenFor("owner", "legacy", "reserved", "owner")
				if err != nil {
					t.Fatal(err)
				}
				// Simulate a persisted bound reservation from an old open project.
				seed.mu.Lock()
				old := seed.data
				p := old.Projects["legacy"]
				p.Mode = "open"
				p.Tokens[0].BoundIdentity = "pc/codex/session"
				p.Tokens[0].FirstConnectedAt = p.Tokens[0].IssuedAt
				old.Projects["legacy"] = p
				old.DisableUnauthenticated = disabled
				before, err := json.Marshal(old)
				seed.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if err := seed.Close(); err != nil {
					t.Fatal(err)
				}
				dir = t.TempDir() // Import into a fresh SQLite store.
				if err := os.WriteFile(filepath.Join(dir, "control.json"), before, 0600); err != nil {
					t.Fatal(err)
				}
				// Other stores (registration and memory) are intentionally outside control state.
				marker := []byte("unchanged registration and memory data")
				if err := os.WriteFile(filepath.Join(dir, "project-data.fixture"), marker, 0600); err != nil {
					t.Fatal(err)
				}
				opener := OpenSQLite
				if storage == "sqlite" {
					db, err := sqlitestore.Open(dir, "control.db")
					if err != nil {
						t.Fatal(err)
					}
					if err := db.Initialize(map[string][]byte{"state": before}); err != nil {
						t.Fatal(err)
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
				}
				p.Mode = "token"
				old.Projects["legacy"] = p
				old.DisableUnauthenticated = true
				for range 2 {
					s, err := opener(dir)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { s.Close() })
					if !reflect.DeepEqual(s.data, old) {
						t.Error("migration changed data beyond mode and legacy deny flag")
					}
					if s.Authorize("legacy", "") || !s.Authorize("legacy", raw) {
						t.Error("migrated authentication incorrect")
					}
					grant, _, ok := s.authorize("legacy", raw)
					if !ok || grant.Check == nil || grant.Claim == nil || grant.Check("pc/codex/session") != nil || grant.Claim("other") == nil {
						t.Error("binding not preserved")
					}
					if _, ok := s.AuthenticateUser("owner", testPassword); !ok {
						t.Error("account password not preserved")
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
				}
				var persisted []byte
				{
					db, e := sqlitestore.Open(dir, "control.db")
					if e != nil {
						t.Fatal(e)
					}
					persisted, err = db.Get("state")
					db.Close()
					after, e := os.ReadFile(filepath.Join(dir, "control.json"))
					if e != nil || !bytes.Equal(before, after) {
						t.Error("SQLite migration modified legacy JSON")
					}
				}
				var stateOnDisk state
				if err != nil || json.Unmarshal(persisted, &stateOnDisk) != nil || !reflect.DeepEqual(stateOnDisk, old) {
					t.Error("migration was not persisted")
				}
				after, err := os.ReadFile(filepath.Join(dir, "project-data.fixture"))
				if err != nil || !bytes.Equal(after, marker) {
					t.Error("project data changed")
				}
			})
		}
	}
}

func TestTokenOnlyHTTPRejectsLegacyFields(t *testing.T) {
	s := userStore(t)
	raw, err := s.IssueTokenFor("owner", "demo", "reserved", "owner")
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, &ownedPanelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, method := range []string{"GET", "POST", "DELETE"} {
		for _, token := range []string{"", "invalid", raw} {
			r := httptest.NewRequest(method, "https://broker/mcp?project=demo", nil)
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 401
			if token == raw {
				want = 204
			}
			if w.Code != want {
				t.Errorf("%s valid=%v got=%d", method, token == raw, w.Code)
			}
		}
	}
}

func TestTokenOnlyMigrationFailureDoesNotPublishPartialState(t *testing.T) {
	s := userStore(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.data.Projects["demo"]
	p.Mode = "open"
	s.data.Projects["demo"] = p
	s.data.DisableUnauthenticated = false
	before, err := json.Marshal(s.data)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	s.write = func([]byte) error { writes++; return errors.New("injected migration write failure") }
	if err := s.migrateTokenOnly(); !errors.Is(err, ErrStorage) {
		t.Fatalf("migration = %v", err)
	}
	after, err := json.Marshal(s.data)
	if err != nil || !bytes.Equal(before, after) || writes != 1 {
		t.Error("failed migration published partial state")
	}
}

func TestTokenOnlyJSONImportPersistsMissingLegacyFlag(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProject("existing", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	legacy := s.data
	legacy.DisableUnauthenticated = false
	before, err := json.Marshal(legacy)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir() // Import into a fresh SQLite store.
	if err := os.WriteFile(filepath.Join(dir, "control.json"), before, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := readStoredControlState(dir)
	var persisted state
	if err != nil || json.Unmarshal(after, &persisted) != nil || !persisted.DisableUnauthenticated {
		t.Fatal("missing legacy flag was not persisted as disabled")
	}
}
