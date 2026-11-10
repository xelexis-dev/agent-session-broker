package controlpanel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"reflect"
	"testing"
)

func TestProjectDeletionGenerationRetryFence(t *testing.T) {
	s := userStore(t)
	g1 := s.ProjectGeneration("demo")
	if _, err := s.beginProjectDeletionGuarded("admin", "demo", "demo", g1, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.finishProjectDeletion("demo", g1); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProjectFor("admin", "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueProjectTokenFor("admin", "demo", "new-generation"); err != nil {
		t.Fatal(err)
	}
	before := s.data.Projects["demo"]
	if _, err := s.beginProjectDeletionGuarded("admin", "demo", "demo", g1, nil); !errors.Is(err, errProjectGenerationMismatch) {
		t.Fatal("stale delete accepted instead of generation mismatch")
	}
	after := s.data.Projects["demo"]
	if after.Deleting || !after.Enabled || len(after.Tokens) != len(before.Tokens) || after.ProjectGenerationID != before.ProjectGenerationID {
		t.Fatal("stale retry changed recreated project")
	}
}

func TestCommitGuardsAbortWithoutStateChange(t *testing.T) {
	for _, name := range []string{"create-project", "save-project", "member", "issue", "revoke", "scopes", "create-user", "update-user", "http-policy", "scope-policy", "attachments", "device", "delete", "session-admission"} {
		t.Run(name, func(t *testing.T) {
			s := userStore(t)
			raw, err := s.IssueProjectTokenFor("owner", "demo", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			parent := scopeEditParent(t, s, raw)
			device, err := s.BeginDeviceAuthorization("demo", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "synthetic", InstanceID: "pc-fixture", RequestID: "fixture-request"})
			if err != nil {
				t.Fatal(err)
			}
			view, err := s.TokenScopeEditFor("owner", "demo", parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(s.data)
			writes := 0
			batch := s.writeBatch
			s.writeBatch = func(puts map[string][]byte, deletes []string) error { writes++; return batch(puts, deletes) }
			blocked := errors.New("fixture guard refused")
			calls := 0
			guard := commitGuard(func() error {
				calls++
				if s.mu.TryLock() {
					s.mu.Unlock()
					t.Error("guard ran without Store.mu")
				}
				return blocked
			})
			switch name {
			case "create-project":
				err = s.saveProjectForGuarded("admin", "new", "token", true, true, false, guard)
			case "save-project":
				err = s.saveProjectForGuarded("owner", "demo", "token", false, false, false, guard)
			case "member":
				_, err = s.changeProjectMemberGuarded("owner", "demo", "developer", "viewer", "update", guard)
			case "issue":
				_, _, err = s.issueProjectTokenGuarded("owner", "demo", "new", ProjectTokenOptions{}, guard)
			case "revoke":
				err = s.revokeTokenForGuarded("owner", "demo", parent.ID, guard)
			case "scopes":
				_, err = s.editTokenScopesForGuarded("owner", "demo", parent.ID, []string{access.SessionRegister}, []string{view.Version}, guard)
			case "create-user":
				err = s.createUserGuarded("admin", "new-user", testPassword, "user", guard)
			case "update-user":
				err = s.updateUserGuarded("admin", "developer", "user", false, "", func(userChange) error { return guard() })
			case "http-policy":
				err = s.setInstanceHTTPPolicyGuarded("admin", !s.data.AllowHTTP, guard)
			case "scope-policy":
				err = s.setScopeIssuanceReadyGuarded("admin", !bool(s.data.ScopeIssuanceReady), guard)
			case "attachments":
				err = s.setAttachmentPolicyGuarded("admin", !s.attachmentEnabled, guard)
			case "device":
				_, err = s.approveDeviceAuthorizationGuarded("owner", device.UserCode, "fixture-agent", false, guard)
			case "delete":
				_, err = s.beginProjectDeletionGuarded("owner", "demo", "demo", s.ProjectGeneration("demo"), guard)
			case "session-admission":
				_, err = s.startProjectOperationGuarded("demo", nil, guard)
			}
			after, _ := json.Marshal(s.data)
			if !errors.Is(err, blocked) || calls != 1 || writes != 0 || !bytes.Equal(before, after) || len(s.operations) != 0 || s.attachmentEnabled {
				t.Fatal("guard did not abort atomically")
			}
		})
	}
}

func TestUpdateUserGuardClassifiesFinalMixedDiff(t *testing.T) {
	s := userStore(t)
	if err := s.CreateUser("admin", "mixed", testPassword, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser("admin", "mixed", "admin", false, ""); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("reauth required")
	var change userChange
	err := s.updateUserGuarded("admin", "mixed", "user", true, testPassword+"-changed", func(c userChange) error { change = c; return refused })
	if !errors.Is(err, refused) || !change.RaisesPower || !change.PasswordSet || change.Before.Role != "admin" || change.After.Role != "user" || change.Before.Enabled || !change.After.Enabled {
		t.Fatal("mixed demotion hid credential restoration")
	}
	if u, _ := s.User("mixed"); u.Role != "admin" || u.Enabled {
		t.Fatal("refused update committed")
	}
}

func TestProjectTokenIssuerReturnsCommittedMetadata(t *testing.T) {
	s := userStore(t)
	raw, metadata, err := s.issueProjectTokenGuarded("owner", "demo", " same-label ", ProjectTokenOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored := scopeEditParent(t, s, raw)
	if !reflect.DeepEqual(metadata, projectTokenProjection(s.data.Projects["demo"], stored)) || metadata.Label != "same-label" || metadata.ID == "" || metadata.Admission != "manual" || metadata.IssuedAt.IsZero() {
		t.Fatal("issuance metadata differs from stored commit")
	}
	metadata.Scopes[0] = "fixture-change"
	if reflect.DeepEqual(metadata.Scopes, scopeEditParent(t, s, raw).Scopes) {
		t.Fatal("metadata aliases stored scopes")
	}
}

func TestDeleteProjectGuardRunsOnlyAtBeginAndRefusesOldGeneration(t *testing.T) {
	s := userStore(t)
	b := &deletingBackend{}
	p := &panel{store: s, backend: b}
	g1 := s.ProjectGeneration("demo")
	calls := 0
	if err := p.deleteProjectAs(context.Background(), "owner", "demo", "demo", g1, func() error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(b.deleted) != 1 {
		t.Fatal("delete guard rechecked at finish")
	}
	if err := s.CreateProjectFor("owner", "demo"); err != nil {
		t.Fatal(err)
	}
	if err := p.deleteProjectAs(context.Background(), "owner", "demo", "demo", g1, nil); !errors.Is(err, errProjectGenerationMismatch) || len(b.deleted) != 1 {
		t.Fatal("stale delete reached backend")
	}
}

func TestUserLockedCoresReuseSinglePasswordSlot(t *testing.T) {
	s := userStore(t)
	s.hashSlot <- struct{}{}
	defer func() { <-s.hashSlot }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.createUserLocked("admin", "reauth-user", testPassword, "user", nil); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := s.updateUserLocked("admin", "reauth-user", "admin", true, "", func(c userChange) error {
		called = true
		if !c.RaisesPower || c.After.Revision != c.Before.Revision+1 {
			t.Error("final user diff is incomplete")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("locked user update skipped guard")
	}
}
