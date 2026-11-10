package broker

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"testing"
)

func TestManagementStatePersistsAndSuspendedDetachedRulesRemain(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLitePersistentRemoteService(dir)
	if err != nil {
		t.Fatal(err)
	}
	request := RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", ClientID: "manage-client", PID: 123, CWD: "/work", Name: "target", ChannelID: "manage-lease", Secret: "synthetic-long-resume-secret-0123456789", RequestID: "manage-register"}
	result, err := s.RegisterSession(t.Context(), request)
	if err != nil || result.Code != "" {
		t.Fatal("register setup", err, result.Code)
	}
	check := func(Session, string, []Session) (protocol.ManagementEvent, error) {
		return protocol.ManagementEvent{ActorName: "manager", ActorRegistrationID: "actor-registration"}, nil
	}
	id := result.Session.RegistrationID
	if err := s.ManageSession(t.Context(), id, "suspended", check, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ManageSession(t.Context(), id, "detached", check, nil); err != ErrSessionSuspended {
		t.Fatal("suspended detach allowed", err)
	}
	if err := s.ManageSession(t.Context(), id, "unsuspended", check, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewSQLitePersistentRemoteService(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	current, err := s.GetSession(context.Background(), id)
	if err != nil || current.LastManagement == nil || current.LastManagement.Kind != "unsuspended" || current.LastManagement.ActorName != "manager" {
		t.Fatal("last management did not survive restart")
	}
	if err := s.ManageSession(t.Context(), id, "detached", check, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ManageSession(t.Context(), id, "suspended", check, nil); err == nil {
		t.Fatal("detached target suspended")
	}
}
