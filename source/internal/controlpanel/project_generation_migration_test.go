package controlpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
	"maps"
	"path/filepath"
	"reflect"
	"testing"
)

func generationV4(t *testing.T, kind string) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "empty" {
		if s.Setup(testPassword) != nil || s.CreateProject("demo") != nil {
			t.Fatal("fixture failed")
		}
		if s.SetScopeIssuanceReady("admin", true) != nil {
			t.Fatal("scope history failed")
		}
		raw, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "restricted", ProjectTokenOptions{Scopes: []string{access.SessionRegister, access.SessionRegisterAuto, access.SessionRead}})
		if err != nil {
			t.Fatal(err)
		}
		req := DirectRequest{DeviceRequest: DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", InstanceID: "client-generation-test", RequestID: "generation-issued"}, RequestedNickname: "worker"}
		if _, err = s.ExchangeDirect("demo", raw, req, DirectProof{}, "fixture", false); err != nil {
			t.Fatal("auto fixture failed")
		}
		if kind == "deleting" {
			if _, err = s.beginProjectDeletion("admin", "demo", "demo"); err != nil {
				t.Fatal(err)
			}
		}
	}
	old := s.data
	old.Version = 4
	old.Projects = maps.Clone(old.Projects)
	for name, p := range old.Projects {
		p.ProjectGenerationID = ""
		old.Projects[name] = p
	}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "large" {
		raw = append(raw, bytes.Repeat([]byte(" "), legacyControlMaxBytes)...)
	}
	s.Close()
	db, err := sqlitestore.Open(dir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Put("state", raw); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return dir, raw
}

func TestProjectGenerationV4FixturesAndStableRestart(t *testing.T) {
	for _, kind := range []string{"scopes-auto-history", "large", "empty", "deleting"} {
		t.Run(kind, func(t *testing.T) {
			dir, raw := generationV4(t, kind)
			var before state
			if err := decodeSQLiteControlState(raw, &before); err != nil {
				t.Fatal("valid v4 rejected before migration")
			}
			s, err := OpenSQLite(dir)
			if err != nil {
				t.Fatal("v4 migration failed")
			}
			if s.data.Version != 5 {
				t.Fatal("version not migrated")
			}
			ids := map[string]string{}
			for name, p := range s.data.Projects {
				ids[name] = p.ProjectGenerationID
				if !registrationNickname.MatchString(p.ProjectGenerationID) {
					t.Fatal("missing generation")
				}
				prior := before.Projects[name]
				p.ProjectGenerationID = ""
				if !reflect.DeepEqual(p.Tokens, prior.Tokens) || !reflect.DeepEqual(p.AutoBindings, prior.AutoBindings) || !reflect.DeepEqual(p.AutoExchanges, prior.AutoExchanges) || p.Deleting != prior.Deleting {
					t.Fatal("migration rewrote v4 token/binding/deletion state")
				}
			}
			if !reflect.DeepEqual(s.data.ScopeIssuanceChanges, before.ScopeIssuanceChanges) {
				t.Fatal("scope history changed")
			}
			s.Close()
			s, err = OpenSQLite(dir)
			if err != nil {
				t.Fatal("v5 restart failed")
			}
			defer s.Close()
			for name, id := range ids {
				if s.data.Projects[name].ProjectGenerationID != id {
					t.Fatal("restart generated a second identity")
				}
			}
		})
	}
}

func TestProjectGenerationMigrationWriteCrashBoundaries(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-write", true: "after-commit-before-publish"}[durable], func(t *testing.T) {
			dir, raw := generationV4(t, "scopes-auto-history")
			s, err := openControlRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = decodeSQLiteControlState(raw, &s.data); err != nil {
				t.Fatal(err)
			}
			db, err := sqlitestore.Open(filepath.Clean(dir), "control.db")
			if err != nil {
				t.Fatal(err)
			}
			s.closeDatabase = db.Close
			s.writeBatch = db.Batch
			var committed state
			s.write = func(candidate []byte) error {
				json.Unmarshal(candidate, &committed)
				if durable {
					if err := db.Put("state", candidate); err != nil {
						return err
					}
				}
				return errors.New("synthetic interruption")
			}
			if err = s.migrateProjectGenerations(); err == nil || s.data.Version != 4 || s.data.Projects["demo"].ProjectGenerationID != "" {
				t.Fatal("failed migration published partial state")
			}
			s.Close()
			recovered, err := OpenSQLite(dir)
			if err != nil {
				t.Fatal("restart after interruption failed")
			}
			id := recovered.ProjectGeneration("demo")
			if id == "" || recovered.data.Version != 5 {
				t.Fatal("restart did not complete atomic migration")
			}
			if durable && id != committed.Projects["demo"].ProjectGenerationID {
				t.Fatal("committed identity replaced after crash")
			}
			recovered.Close()
		})
	}
}
