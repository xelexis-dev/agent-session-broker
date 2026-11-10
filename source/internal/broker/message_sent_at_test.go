package broker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestRemoteAcceptanceStampsUTCIncludingAnnouncements(t *testing.T) {
	s := NewRemoteService()
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 10, 18, 10, 20, 30, 123456789, time.FixedZone("fixture", 9*3600))
	s.now = func() time.Time { return now }
	id := remoteIdentity(1)
	id.Capabilities = []string{protocol.MessageSentAtCapability}
	if _, err := s.AttachSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for _, admin := range []bool{false, true} {
		var err error
		if admin {
			_, err = s.SendAnnouncement(t.Context(), id.Name, "notice")
		} else {
			_, err = s.SendMessage(t.Context(), Message{From: id.Name, To: id.Name, Body: "fixture", SentAt: time.Unix(1, 0), RegistrationID: "forged"})
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.WaitForIngress(t.Context(), id.Name, id.ChannelID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.SentAt.Equal(now.UTC().Truncate(time.Millisecond)) || got.SentAt.Location() != time.UTC || got.RegistrationID != "" {
			t.Fatalf("unstamped or forged routing: %+v", got)
		}
	}
}

func TestRemoteEnvelopeLimitIncludesSentAt(t *testing.T) {
	s := NewRemoteService()
	t.Cleanup(func() { s.Close() })
	id := remoteIdentity(1)
	if _, err := s.AttachSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	m := Message{From: id.Name, To: id.Name}
	raw, _ := json.Marshal(m)
	m.Body = strings.Repeat("x", protocol.MaxMessageBytes-len(raw)-len(`,"body":""`))
	raw, _ = json.Marshal(m)
	if len(raw) != protocol.MaxMessageBytes {
		t.Fatalf("bad boundary fixture %d", len(raw))
	}
	result, err := s.SendMessage(t.Context(), m)
	if err == nil || result.Accepted || err.Error() != "message requires a body and an envelope of at most 12000 bytes" {
		t.Fatal("accepted envelope whose timestamp exceeds the limit")
	}
	state, _ := s.GetSession(t.Context(), id.Name)
	if state.PendingMessages != 0 {
		t.Fatal("oversized envelope was queued")
	}
}

func TestCapabilityFollowsDurableLeaseAndResetsOnRestart(t *testing.T) {
	root := t.TempDir()
	s, _ := clockedService(t, root)
	r := durableRegister(1, "fixture", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	r.Capabilities = []string{protocol.MessageSentAtCapability}
	first, err := s.RegisterSession(t.Context(), r)
	if err != nil || first.Code != "" {
		t.Fatal("register failed")
	}
	send := func(service *RemoteService, lease string, want bool) {
		t.Helper()
		if _, err := service.SendMessage(t.Context(), Message{From: r.Name, To: r.Name, Body: "fixture"}); err != nil {
			t.Fatal(err)
		}
		got, err := service.WaitForIngress(t.Context(), r.Name, lease)
		if err != nil || (!got.SentAt.IsZero()) != want {
			t.Fatal("capability not bound to current lease")
		}
	}
	send(s, "lease-1", true)
	old := resumeFrom(r, first.Session.RegistrationID, "lease-2", "req-2", first.Session.Revision)
	second, err := s.ResumeSession(t.Context(), old)
	if err != nil || second.Code != "" {
		t.Fatal("old resume failed")
	}
	send(s, "lease-2", false)
	modern := resumeFrom(r, first.Session.RegistrationID, "lease-3", "req-3", second.Session.Revision)
	modern.Capabilities = r.Capabilities
	third, err := s.ResumeSession(t.Context(), modern)
	if err != nil || third.Code != "" {
		t.Fatal("modern resume failed")
	}
	send(s, "lease-3", true)
	// A replay of the previous lease cannot downgrade the current receiver.
	if _, err := s.ResumeSession(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	send(s, "lease-3", true)
	s.Close()
	restarted, _ := clockedService(t, root)
	if restarted.byRegistration[first.Session.RegistrationID].messageSentAt {
		t.Fatal("capability persisted across restart")
	}
	last := resumeFrom(r, first.Session.RegistrationID, "lease-4", "req-4", third.Session.Revision)
	last.Capabilities = r.Capabilities
	result, err := restarted.ResumeSession(t.Context(), last)
	if err != nil || result.Code != "" {
		t.Fatal("restart resume failed")
	}
	send(restarted, "lease-4", true)
}
