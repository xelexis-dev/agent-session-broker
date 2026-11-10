package broker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type ownerTestAdmission struct{ owner string }

func (a *ownerTestAdmission) Admit(_ context.Context, r RegisterRequest) (string, string, error) {
	return r.Name, "", nil
}
func (a *ownerTestAdmission) RegistrationOwner(context.Context) string { return a.owner }

func TestRegistrationOwnerPersistsTransfersAndBackfills(t *testing.T) {
	root := t.TempDir()
	s, err := NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	admission := &ownerTestAdmission{owner: "alice"}
	s.SetAdmission(admission)
	req := durableRegister(1, "owned", "client-owner", durableSecretA, "lease-a", "request-a")
	out, err := s.RegisterSession(t.Context(), req)
	if err != nil || out.Code != "" {
		t.Fatal("register failed", err, out.Code)
	}
	id := out.Session.RegistrationID
	if s.RegistrationOwners()[id] != "alice" {
		t.Fatal("owner not recorded")
	}
	admission.owner = "bob"
	req.ExpectedRevision = out.Session.Revision
	req.ChannelID = "lease-b"
	req.RequestID = "request-b"
	out, err = s.RegisterSession(t.Context(), req)
	if err != nil || out.Code != "" {
		t.Fatal("reapproval failed", err, out.Code)
	}
	if s.RegistrationOwners()[id] != "bob" {
		t.Fatal("latest approval did not transfer owner")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.RegistrationOwners()[id] != "bob" {
		t.Fatal("restart lost ownership")
	}
	identity, _ := json.Marshal([3]string{req.Harness, req.SessionID, req.ClientID})
	if err := s.BackfillRegistrationOwners(map[string]string{string(identity): "alice"}); err != nil {
		t.Fatal(err)
	}
	if s.RegistrationOwners()[id] != "bob" {
		t.Fatal("backfill replaced recorded owner")
	}
	legacy, err := s.RegisterSession(t.Context(), durableRegister(2, "legacy", "client-legacy", durableSecretA, "lease-c", "request-c"))
	if err != nil || legacy.Code != "" {
		t.Fatal("legacy fixture failed")
	}
	legacyIdentity, _ := json.Marshal([3]string{legacy.Session.Harness, legacy.Session.ID, legacy.Session.ClientID})
	if err := s.BackfillRegistrationOwners(map[string]string{string(legacyIdentity): "carol"}); err != nil {
		t.Fatal(err)
	}
	if s.RegistrationOwners()[legacy.Session.RegistrationID] != "carol" {
		t.Fatal("legacy owner not backfilled")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.RegistrationOwners()[legacy.Session.RegistrationID] != "carol" {
		t.Fatal("backfill not durable")
	}

}

func TestRegistrationOwnerBackfillStorageFailureDoesNotPublish(t *testing.T) {
	s, err := NewSQLitePersistentRemoteService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	req := durableRegister(1, "legacy", "client-owner", durableSecretA, "lease", "request")
	out, err := s.RegisterSession(t.Context(), req)
	if err != nil || out.Code != "" {
		t.Fatal("fixture failed")
	}
	s.store.writeFile = func(string, []byte) error { return errors.New("fixture write failure") }
	identity, _ := json.Marshal([3]string{req.Harness, req.SessionID, req.ClientID})
	if err := s.BackfillRegistrationOwners(map[string]string{string(identity): "alice"}); err == nil {
		t.Fatal("failure hidden")
	}
	if s.RegistrationOwners()[out.Session.RegistrationID] != "" {
		t.Fatal("failed owner write published")
	}
}
