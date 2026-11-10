package broker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMisuseA2ExactMultilingualEnvelopeBoundary(t *testing.T) {
	s, now := clockedService(t, "")
	*now = now.Add(123 * time.Millisecond)
	req := durableRegister(1, "alpha", "misuse-client", durableSecretA, "lease", "request")
	registered, err := s.RegisterSession(t.Context(), req)
	if err != nil || registered.Code != "" {
		t.Fatal("fixture failed")
	}
	base := Message{FromRegistrationID: registered.Session.RegistrationID, From: "alpha", To: "alpha", RegistrationID: registered.Session.RegistrationID, SentAt: now.UTC(), Body: "x"}
	raw, _ := json.Marshal(base)
	budget := 12000 - len(raw) + 1
	base.Body = strings.Repeat("가", budget/3) + strings.Repeat("x", budget%3)
	exact, _ := json.Marshal(base)
	if len(exact) != 12000 {
		t.Fatal("test did not construct exact wire boundary")
	}
	if sent, err := s.SendMessage(t.Context(), base); err != nil || !sent.Accepted {
		t.Fatal("12000-byte multilingual envelope rejected")
	}
	base.Body += "x"
	if sent, err := s.SendMessage(t.Context(), base); err == nil || sent.Accepted {
		t.Fatal("12001-byte multilingual envelope accepted")
	}
	got, _ := s.GetSession(t.Context(), "alpha")
	if got.PendingMessages != 1 {
		t.Fatal("rejected envelope changed queue")
	}
}

func TestMisuseA1InvisibleNamesCannotEnterViaRegistrationOrAttach(t *testing.T) {
	for _, name := range []string{"zero\u200bwidth", "rtl\u202ename", "isolate\u2066name", "bom\ufeffname"} {
		s, _ := clockedService(t, "")
		req := durableRegister(1, name, "misuse-client", durableSecretA, "lease", "request")
		if got, err := s.RegisterSession(t.Context(), req); err == nil && got.Code == "" {
			t.Error("registration accepted invisible Unicode format character")
		}
		legacy, _ := clockedService(t, "")
		if _, err := legacy.AttachSession(t.Context(), AttachRequest{Harness: req.Harness, SessionID: sessionUUID(2), PID: req.PID, CWD: req.CWD, ChannelID: "legacy-lease", Name: name}); err == nil {
			t.Error("legacy attach accepted invisible Unicode format character")
		}
	}
}

func TestMisuseA1HistoricalInvisibleNameStillResumesAndCanBeCorrected(t *testing.T) {
	root := t.TempDir()
	s, _ := clockedService(t, root)
	req := durableRegister(1, "visible", "misuse-client", durableSecretA, "lease", "request")
	out, err := s.RegisterSession(t.Context(), req)
	if err != nil || out.Code != "" {
		t.Fatal("fixture failed")
	}
	// Seed the old on-disk format directly, as a pre-fix broker could persist it.
	s.mu.Lock()
	historical := *s.byRegistration[out.Session.RegistrationID].record
	historical.Name = "legacy\u200bname"
	err = s.persistLocked(&historical)
	s.mu.Unlock()
	if err != nil {
		t.Fatal("historical fixture write failed")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := newRemoteService(root, false)
	if err != nil {
		t.Fatal("historical record blocked startup")
	}
	defer restored.Close()
	resumed, err := restored.ResumeSession(t.Context(), resumeFrom(req, out.Session.RegistrationID, "new-lease", "resume", out.Session.Revision))
	if err != nil || resumed.Code != "" || resumed.Session.Name != historical.Name {
		t.Fatal("historical name could not resume")
	}
	fixed, err := restored.RenameSession(t.Context(), RenameRequest{RegistrationID: out.Session.RegistrationID, ChannelID: "new-lease", Name: "visible-again"})
	if err != nil || fixed.Name != "visible-again" {
		t.Fatal("historical name could not be corrected")
	}
}
