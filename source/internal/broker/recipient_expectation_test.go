package broker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestRecipientExpectationRejectsNameReusedByAnotherRegistration(t *testing.T) {
	s, _ := clockedService(t, "")
	sender := durableRegister(951, "sender", "sender-client", durableSecretA, "sender-lease", "sender-request")
	sent, _ := s.RegisterSession(t.Context(), sender)
	a := durableRegister(952, "recipient", "first-client", durableSecretA, "first-lease", "first-request")
	first, err := s.RegisterSession(t.Context(), a)
	if err != nil || first.Code != "" || sent.Code != "" {
		t.Fatal("fixture registration")
	}
	if _, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: first.Session.RegistrationID, ChannelID: a.ChannelID, Name: "recipient-moved"}); err != nil {
		t.Fatal("fixture rename")
	}
	b := durableRegister(953, "recipient", "second-client", durableSecretB, "second-lease", "second-request")
	second, err := s.RegisterSession(t.Context(), b)
	if err != nil || second.Code != "" {
		t.Fatal("fixture name reuse")
	}
	raw, _ := json.Marshal(map[string]any{"from": sender.Name, "to": "recipient", "body": "must-not-deliver", "expectRegistrationId": first.Session.RegistrationID})
	var message Message
	if json.Unmarshal(raw, &message) != nil {
		t.Fatal("fixture message")
	}
	result, err := s.SendMessage(t.Context(), message)
	encoded, _ := json.Marshal(result)
	var output map[string]any
	_ = json.Unmarshal(encoded, &output)
	if err != nil || result.Accepted || output["code"] != "recipient-changed" {
		t.Fatal("expected old registration was delivered to the reused name")
	}
	if len(s.byRegistration[second.Session.RegistrationID].queue) != 0 {
		t.Fatal("recipient mismatch enqueued a message")
	}
}

func TestRecipientExpectationScopeValidationAndNondurable(t *testing.T) {
	s, _ := clockedService(t, "")
	a := durableRegister(954, "source", "source-client", durableSecretA, "source-lease", "source-request")
	b := durableRegister(955, "target", "target-client", durableSecretB, "target-lease", "target-request")
	s.RegisterSession(t.Context(), a)
	target, _ := s.RegisterSession(t.Context(), b)
	wrong := strings.Repeat("A", 26)
	for _, read := range []bool{false, true} {
		scopes := []string{access.MessageSend}
		if read {
			scopes = append(scopes, access.SessionRead)
		}
		ctx := access.WithGrant(t.Context(), access.Grant{Scopes: scopes})
		got, err := s.SendMessage(ctx, Message{From: a.Name, To: b.Name, Body: "mismatch", ExpectRegistrationID: &wrong})
		if err != nil || got.Code != protocol.CodeRecipientChanged || got.Accepted || (got.CurrentRegistrationID != "") != read {
			t.Fatal("recipient mismatch disclosure changed")
		}
		if read && got.CurrentRegistrationID != target.Session.RegistrationID {
			t.Fatal("wrong current registration")
		}
	}
	for _, bad := range []string{"", "lowercase", strings.Repeat("A", 25), strings.Repeat("A", 27)} {
		got, err := s.SendMessage(t.Context(), Message{From: a.Name, To: b.Name, Body: "invalid", ExpectRegistrationID: &bad})
		if err != nil || got.Code != CodeInvalid || got.Accepted {
			t.Fatal("invalid expectation accepted")
		}
	}
	legacy := AttachRequest{Harness: "codex", Name: "legacy", SessionID: sessionUUID(956), PID: 956, CWD: "/synthetic", ChannelID: "legacy-lease"}
	if _, err := s.AttachSession(t.Context(), legacy); err != nil {
		t.Fatal("legacy fixture")
	}
	expected := target.Session.RegistrationID
	got, err := s.SendMessage(t.Context(), Message{From: a.Name, To: legacy.Name, Body: "cannot-pin-legacy", ExpectRegistrationID: &expected})
	if err != nil || got.Code != protocol.CodeRecipientChanged || got.Accepted || got.CurrentRegistrationID != "" {
		t.Fatal("legacy recipient satisfied expectation")
	}
	got, err = s.SendMessage(t.Context(), Message{From: a.Name, To: b.Name, Body: "legacy-name-only"})
	if err != nil || !got.Accepted {
		t.Fatal("name-only behavior changed")
	}
}
func TestFromRegistrationIsFrozenAndSpoofIsIgnored(t *testing.T) {
	s, _ := clockedService(t, "")
	a := durableRegister(957, "source-freeze", "source-client", durableSecretA, "source-lease", "source-request")
	b := durableRegister(958, "target-freeze", "target-client", durableSecretB, "target-lease", "target-request")
	source, _ := s.RegisterSession(t.Context(), a)
	target, _ := s.RegisterSession(t.Context(), b)
	expected := target.Session.RegistrationID
	if got, err := s.SendMessage(t.Context(), Message{From: a.Name, To: b.Name, Body: "frozen", FromRegistrationID: "forged", ExpectRegistrationID: &expected}); err != nil || !got.Accepted {
		t.Fatal("matching expectation rejected")
	}
	s.RenameSession(t.Context(), RenameRequest{RegistrationID: source.Session.RegistrationID, ChannelID: a.ChannelID, Name: "new-source-name"})
	if err := s.StopSession(t.Context(), "new-source-name"); err != nil {
		t.Fatal("source detach")
	}
	message, err := s.WaitForIngress(t.Context(), b.Name, b.ChannelID)
	if err != nil || message.From != a.Name || message.FromRegistrationID != source.Session.RegistrationID || message.RegistrationID != target.Session.RegistrationID || message.ExpectRegistrationID != nil {
		t.Fatal("frozen source or recipient registration changed")
	}
	if _, err := s.SendAnnouncement(t.Context(), b.Name, "system"); err != nil {
		t.Fatal("announcement")
	}
	system, err := s.WaitForIngress(t.Context(), b.Name, b.ChannelID)
	if err != nil || system.FromRegistrationID != "" {
		t.Fatal("system sender acquired a registration ID")
	}
}
func TestSourceRegistrationBatchAndEnvelopeBudget(t *testing.T) {
	s, now := clockedService(t, "")
	a := durableRegister(959, "size-source", "source-client", durableSecretA, "source-lease", "source-request")
	b := durableRegister(960, "size-target", "target-client", durableSecretB, "target-lease", "target-request")
	source, _ := s.RegisterSession(t.Context(), a)
	target, _ := s.RegisterSession(t.Context(), b)
	m := Message{From: a.Name, To: b.Name, Body: "x", SentAt: now.UTC().Truncate(time.Millisecond), FromRegistrationID: source.Session.RegistrationID, RegistrationID: target.Session.RegistrationID}
	raw, _ := json.Marshal(m)
	m.Body = strings.Repeat("x", protocol.MaxMessageBytes-len(raw)+1)
	result, err := s.SendMessage(t.Context(), m)
	if err != nil || !result.Accepted {
		t.Fatal("exact envelope budget rejected")
	}
	m.Body += "x"
	result, err = s.SendMessage(t.Context(), m)
	if err == nil || result.Accepted {
		t.Fatal("source registration bypassed envelope limit")
	}
}

type recipientDenyAdmission struct{}

func (recipientDenyAdmission) Admit(_ context.Context, r RegisterRequest) (string, string, error) {
	return r.Name, "", nil
}
func (recipientDenyAdmission) CheckSession(context.Context, Session) error {
	return errors.New("synthetic sender denied")
}
func TestSenderAuthorizationPrecedesRecipientDisclosure(t *testing.T) {
	s, _ := clockedService(t, "")
	a := durableRegister(961, "auth-source", "source-client", durableSecretA, "source-lease", "source-request")
	b := durableRegister(962, "auth-target", "target-client", durableSecretB, "target-lease", "target-request")
	s.RegisterSession(t.Context(), a)
	s.RegisterSession(t.Context(), b)
	s.SetAdmission(recipientDenyAdmission{})
	wrong := strings.Repeat("A", 26)
	result, err := s.SendMessage(t.Context(), Message{From: a.Name, To: b.Name, Body: "not-authorized", ExpectRegistrationID: &wrong})
	if err == nil || result.Accepted || result.CurrentRegistrationID != "" {
		t.Fatal("recipient mismatch bypassed sender authorization")
	}
}
