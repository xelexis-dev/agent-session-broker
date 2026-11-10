package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const durableSecretA = "secret-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const durableSecretB = "secret-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func durableRegister(n int, name, clientID, secret, lease, request string) RegisterRequest {
	return RegisterRequest{
		Holder:    syntheticReceiverHolder(1, "f"),
		Harness:   "claude",
		SessionID: sessionUUID(n),
		PID:       1000 + n,
		CWD:       `C:\work\project`,
		ClientID:  clientID,
		Name:      name,
		ChannelID: lease,
		Secret:    secret,
		RequestID: request,
	}
}

func sessionUUID(n int) string {
	return fmt.Sprintf("11111111-1111-4111-8111-%012d", n)
}

func resumeFrom(r RegisterRequest, registrationID, lease, request string, expected uint64) ResumeRequest {
	return ResumeRequest{
		Holder:           r.Holder,
		RegistrationID:   registrationID,
		Harness:          r.Harness,
		SessionID:        r.SessionID,
		PID:              r.PID,
		CWD:              r.CWD,
		ClientID:         r.ClientID,
		ChannelID:        lease,
		Secret:           r.Secret,
		RequestID:        request,
		ExpectedRevision: expected,
	}
}

func clockedService(t *testing.T, root string) (*RemoteService, *time.Time) {
	t.Helper()
	s, err := newRemoteService(root, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.bootedAt = now
	s.lastFlush = now
	s.lastExpire = now
	return s, &now
}

func TestDurableRegisterResumeRoundTripHidesSecret(t *testing.T) {
	ctx := context.Background()
	s, now := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, err := s.RegisterSession(ctx, a)
	if err != nil || first.Code != "" || first.Session.RegistrationID == "" || first.Session.Revision != 1 || first.Session.Status != StatusAlive || !first.Session.AutomaticDeliveryReady || first.Session.Name != "pc-a" || first.Session.ClientID != a.ClientID || first.Session.Epoch == "" {
		t.Fatalf("register: %+v %v", first, err)
	}
	encoded, _ := json.Marshal(first)
	if strings.Contains(string(encoded), durableSecretA) || strings.Contains(string(encoded), hashSecret(durableSecretA)) {
		t.Fatalf("register result exposes secret material: %s", encoded)
	}
	if _, err := s.SendMessage(ctx, Message{From: "pc-a", To: "pc-a", Body: "self"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.WaitForIngress(ctx, "pc-a", "lease-1")
	if err != nil || got.Body != "self" {
		t.Fatal(got, err)
	}
	*now = now.Add(10 * time.Second)
	renewed, err := s.RenewLease(ctx, RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: "lease-1"})
	if err != nil || renewed.Code != "" || renewed.Session.Status != StatusAlive {
		t.Fatalf("renew: %+v %v", renewed, err)
	}
	// The model running the register command again keeps the live lease.
	rerun := a
	rerun.RequestID, rerun.ExpectedRevision = "req-1b", 1
	kept, err := s.RegisterSession(ctx, rerun)
	if err != nil || kept.Code != "" || kept.Session.Revision != 2 || kept.Session.Generation != first.Session.Generation {
		t.Fatalf("re-register on the live lease: %+v %v", kept, err)
	}
	if _, err := s.SendMessage(ctx, Message{From: "pc-a", To: "pc-a", Body: "still lease-1"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.WaitForIngress(ctx, "pc-a", "lease-1"); err != nil || got.Body != "still lease-1" {
		t.Fatal(got, err)
	}
	a.ExpectedRevision = 2
	resumed, err := s.ResumeSession(ctx, resumeFrom(a, first.Session.RegistrationID, "lease-2", "req-2", 2))
	if err != nil || resumed.Code != "" || resumed.Session.Revision != 3 || resumed.Session.Generation <= first.Session.Generation || resumed.Session.Status != StatusAlive {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	if _, err := s.WaitForIngress(ctx, "pc-a", "lease-1"); err == nil {
		t.Fatal("old lease still receives after resume")
	}
	if r, _ := s.RenewLease(ctx, RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: "lease-1"}); r.Code != CodeInvalid {
		t.Fatalf("old lease renewed: %+v", r)
	}
	if _, err := s.SendMessage(ctx, Message{From: "pc-a", To: "pc-a", Body: "after resume"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.WaitForIngress(ctx, "pc-a", "lease-2"); err != nil || got.Body != "after resume" {
		t.Fatal(got, err)
	}
}

func TestDurableRegisterRetryIsIdempotentAndWrongSecretIsOwned(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, _ := s.RegisterSession(ctx, a)
	replay, err := s.RegisterSession(ctx, a)
	if err != nil || replay.Code != "" || replay.Session.RegistrationID != first.Session.RegistrationID || replay.Session.Revision != first.Session.Revision {
		t.Fatalf("replayed create changed state: %+v %v", replay, err)
	}
	wrong := a
	wrong.Secret = durableSecretB
	wrong.RequestID = "req-2"
	wrong.ChannelID = "lease-x"
	if r, _ := s.RegisterSession(ctx, wrong); r.Code != CodeOwned {
		t.Fatalf("wrong secret accepted: %+v", r)
	}
	if r, _ := s.ResumeSession(ctx, resumeFrom(wrong, first.Session.RegistrationID, "lease-y", "req-3", 0)); r.Code != CodeOwned {
		t.Fatalf("wrong secret resumed: %+v", r)
	}
	if got, _ := s.GetSession(ctx, "pc-a"); got.Revision != 1 || got.Status != StatusAlive {
		t.Fatalf("rejected attempts changed the registration: %+v", got)
	}
	again := a
	again.RequestID = "req-4"
	again.ChannelID = "lease-2"
	again.ExpectedRevision = 1
	revived, err := s.RegisterSession(ctx, again)
	if err != nil || revived.Code != "" || revived.Session.Revision != 2 || revived.Session.RegistrationID != first.Session.RegistrationID {
		t.Fatalf("owner re-register: %+v %v", revived, err)
	}
	if _, err := s.WaitForIngress(ctx, "pc-a", "lease-1"); err == nil {
		t.Fatal("replaced lease still active")
	}
}

func TestDurableSameSessionOnAnotherClientIsSeparateAndUUIDRoutingIsExplicit(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	b := durableRegister(1, "pc-a", "client-bravo-0002", durableSecretB, "lease-2", "req-2")
	first, _ := s.RegisterSession(ctx, a)
	second, err := s.RegisterSession(ctx, b)
	if err != nil || second.Code != "" || second.Session.RegistrationID == first.Session.RegistrationID {
		t.Fatalf("second client rejected: %+v %v", second, err)
	}
	if !validCanonicalConversationID(second.Session.Name) || second.Session.RequestedName != "pc-a" || second.Warning == "" {
		t.Fatalf("duplicate nickname not replaced by UUID nickname: %+v", second)
	}
	if got, err := s.GetSession(ctx, second.Session.Name); err != nil || got.RegistrationID != second.Session.RegistrationID {
		t.Fatalf("UUID nickname does not route by name: %+v %v", got, err)
	}
	if got, err := s.GetSession(ctx, second.Session.RegistrationID); err != nil || got.Name != second.Session.Name {
		t.Fatalf("registration id does not route: %+v %v", got, err)
	}
	if _, err := s.GetSession(ctx, a.SessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ambiguous session UUID resolved: %v", err)
	}
	if _, err := s.SendMessage(ctx, Message{From: "pc-a", To: second.Session.Name, Body: "to uuid nickname"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.WaitForIngress(ctx, second.Session.Name, "lease-2"); err != nil || got.Body != "to uuid nickname" || got.To != second.Session.Name {
		t.Fatal(got, err)
	}
	listed, _ := s.ListSessions(ctx)
	if len(listed) != 2 {
		t.Fatalf("list: %+v", listed)
	}
}

func TestDurableResumeNeverCreatesAndDetachRequiresExplicitRegister(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	if r, _ := s.ResumeSession(ctx, resumeFrom(a, "AAAAAAAAAAAAAAAAAAAAAAAAAA", "lease-1", "req-0", 0)); r.Code != CodeNotFound {
		t.Fatalf("resume created or mis-coded: %+v", r)
	}
	if listed, _ := s.ListSessions(ctx); len(listed) != 0 {
		t.Fatalf("resume created a registration: %+v", listed)
	}
	first, _ := s.RegisterSession(ctx, a)
	if err := s.StopSession(ctx, "pc-a"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession(ctx, "pc-a")
	if got.ErrorCode != "detached" || got.Status != StatusMissing || got.AutomaticDeliveryReady {
		t.Fatalf("detach state: %+v", got)
	}
	if r, _ := s.ResumeSession(ctx, resumeFrom(a, first.Session.RegistrationID, "lease-2", "req-2", 0)); r.Code != CodeDetached {
		t.Fatalf("detached registration resumed automatically: %+v", r)
	}
	again := a
	again.RequestID, again.ChannelID, again.ExpectedRevision = "req-3", "lease-3", 1
	revived, err := s.RegisterSession(ctx, again)
	if err != nil || revived.Code != "" || revived.Session.Status != StatusAlive || revived.Session.RegistrationID != first.Session.RegistrationID {
		t.Fatalf("explicit register did not revive: %+v %v", revived, err)
	}
	if got, _ := s.GetSession(ctx, "pc-a"); got.ErrorCode != "" {
		t.Fatalf("revived registration keeps detached code: %+v", got)
	}
}

func TestDurableConcurrentResumeUsesExpectedRevision(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, _ := s.RegisterSession(ctx, a)
	id := first.Session.RegistrationID
	one, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-2", "req-2", 1))
	two, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-3", "req-3", 1))
	if one.Code != "" || two.Code != CodeRevisionMismatch {
		t.Fatalf("stale resume replaced newer lease: %+v %+v", one, two)
	}
	replay, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-2", "req-2", 1))
	if replay.Code != "" || replay.Session.Revision != one.Session.Revision || replay.Session.Generation != one.Session.Generation {
		t.Fatalf("replayed resume was not idempotent: %+v", replay)
	}
	if _, err := s.WaitForIngress(ctx, "pc-a", "lease-3"); err == nil {
		t.Fatal("rejected lease became active")
	}
	if r, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-1", "req-4", 2)); r.Code != CodeInvalid {
		t.Fatalf("retired lease id reused: %+v", r)
	}
	if r, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-5", "req-5", 0)); r.Code != CodeRevisionMismatch {
		t.Fatalf("zero expected revision bypassed the fence: %+v", r)
	}
}

func TestDurableRestartRestoresMissingWithNewEpochAndSecretStaysHashed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, now := clockedService(t, root)
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, err := s.RegisterSession(ctx, a)
	if err != nil || first.Code != "" {
		t.Fatal(first, err)
	}
	if _, err := s.SendMessage(ctx, Message{From: "pc-a", To: "pc-a", Body: "queued before restart"}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(root, "*.json"))
	if len(files) != 1 {
		t.Fatalf("registration file: %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), durableSecretA) || !strings.Contains(string(raw), hashSecret(durableSecretA)) {
		t.Fatalf("stored record leaks the secret or lacks its hash: %s", raw)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, _ := clockedService(t, root)
	*now = now.Add(time.Minute)
	restarted.now = func() time.Time { return *now }
	if restarted.epoch == s.epoch {
		t.Fatal("epoch did not change across restart")
	}
	got, err := restarted.GetSession(ctx, "pc-a")
	if err != nil || got.Status != StatusMissing || got.ErrorCode != "server-restarted" || got.AutomaticDeliveryReady || got.PendingMessages != 0 || got.RegistrationID != first.Session.RegistrationID {
		t.Fatalf("restored: %+v %v", got, err)
	}
	if r, _ := restarted.RenewLease(ctx, RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: "lease-1"}); r.Code != CodeInvalid {
		t.Fatalf("stale lease renewed after restart: %+v", r)
	}
	resumed, err := restarted.ResumeSession(ctx, resumeFrom(a, first.Session.RegistrationID, "lease-2", "req-2", 1))
	if err != nil || resumed.Code != "" || resumed.Session.Status != StatusAlive || resumed.Session.Generation <= first.Session.Generation || resumed.Session.Epoch == first.Session.Epoch {
		t.Fatalf("resume after restart: %+v %v", resumed, err)
	}
	if _, err := restarted.SendMessage(ctx, Message{From: "pc-a", To: "pc-a", Body: "after restart"}); err != nil {
		t.Fatal(err)
	}
	if got, err := restarted.WaitForIngress(ctx, "pc-a", "lease-2"); err != nil || got.Body != "after restart" {
		t.Fatal(got, err)
	}
}

func TestDurableProbeLifecycleRetainsUnansweredRegistration(t *testing.T) {
	ctx := context.Background()
	s, now := clockedService(t, t.TempDir())
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, _ := s.RegisterSession(ctx, a)
	id := first.Session.RegistrationID
	*now = now.Add(31 * time.Second)
	if got, _ := s.GetSession(ctx, "pc-a"); got.Status != StatusMissing || got.ProbePending {
		t.Fatalf("expired lease: %+v", got)
	}
	*now = now.Add(59 * time.Minute)
	if got, _ := s.GetSession(ctx, "pc-a"); got.ProbePending {
		t.Fatalf("probe issued before one hour: %+v", got)
	}
	*now = now.Add(time.Minute)
	got, _ := s.GetSession(ctx, "pc-a")
	if !got.ProbePending || !got.ProbeDeadline.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("probe not issued at one hour: %+v", got)
	}
	// A late client answer for a probe of an older revision is rejected.
	resumed, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-2", "req-2", 1))
	if resumed.Code != "" || resumed.PendingProbe == nil || resumed.PendingProbe.Revision != resumed.Session.Revision {
		t.Fatalf("resume did not hand over the pending probe with the current revision: %+v", resumed)
	}
	stale, _ := s.AnswerProbe(ctx, ProbeAnswer{RegistrationID: id, ProbeID: resumed.PendingProbe.ID, Revision: resumed.PendingProbe.Revision - 1, ChannelID: "lease-2", OK: true})
	if stale.Code != CodeStaleProbe {
		t.Fatalf("stale probe answer accepted: %+v", stale)
	}
	foreign, _ := s.AnswerProbe(ctx, ProbeAnswer{RegistrationID: id, ProbeID: resumed.PendingProbe.ID, Revision: resumed.PendingProbe.Revision, ChannelID: "lease-1", OK: true})
	if foreign.Code != CodeStaleProbe {
		t.Fatalf("probe answered on a retired lease: %+v", foreign)
	}
	answered, _ := s.AnswerProbe(ctx, ProbeAnswer{RegistrationID: id, ProbeID: resumed.PendingProbe.ID, Revision: resumed.PendingProbe.Revision, ChannelID: "lease-2", OK: true, PID: a.PID, CWD: a.CWD})
	if answered.Code != "" || answered.Removed || answered.Session.ProbePending {
		t.Fatalf("probe answer: %+v", answered)
	}
	// Renewals keep the registration alive; a renewal at 59:59 blocks cleanup.
	*now = now.Add(20 * time.Second)
	if r, _ := s.RenewLease(ctx, RenewRequest{RegistrationID: id, ChannelID: "lease-2"}); r.Code != "" {
		t.Fatalf("renew: %+v", r)
	}
	*now = now.Add(31 * time.Second)
	*now = now.Add(time.Hour)
	if got, _ := s.GetSession(ctx, "pc-a"); !got.ProbePending {
		t.Fatalf("second probe missing: %+v", got)
	}
	*now = now.Add(10*time.Minute - time.Second)
	if _, err := s.GetSession(ctx, "pc-a"); err != nil {
		t.Fatalf("removed before the deadline: %v", err)
	}
	*now = now.Add(2 * time.Second)
	if got, err := s.GetSession(ctx, "pc-a"); err != nil || got.Status != StatusMissing || got.ErrorCode != "probe-unanswered" {
		t.Fatalf("unanswered registration not retained: %+v %v", got, err)
	}
	resumedAgain, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-3", "req-3", resumed.Session.Revision))
	if resumedAgain.Code != "" || resumedAgain.Session.RegistrationID != id || resumedAgain.Session.Name != "pc-a" || resumedAgain.PendingProbe == nil || !resumedAgain.PendingProbe.Deadline.After(*now) {
		t.Fatalf("retained registration did not resume with a fresh probe: %+v", resumedAgain)
	}
	answer, _ := s.AnswerProbe(ctx, ProbeAnswer{RegistrationID: id, ProbeID: resumedAgain.PendingProbe.ID, Revision: resumedAgain.PendingProbe.Revision, ChannelID: "lease-3", OK: true})
	if answer.Code != "" || answer.Session.ProbePending {
		t.Fatalf("fresh probe answer: %+v", answer)
	}
	if files, _ := filepath.Glob(filepath.Join(s.store.root, "*.json")); len(files) != 1 {
		t.Fatalf("retained registration lost file: %v", files)
	}
}

func TestDurableProbeMismatchRetainsAndDetachedExpires(t *testing.T) {
	ctx := context.Background()
	s, now := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, _ := s.RegisterSession(ctx, a)
	id := first.Session.RegistrationID
	*now = now.Add(31*time.Second + time.Hour)
	got, _ := s.GetSession(ctx, "pc-a")
	if !got.ProbePending {
		t.Fatalf("probe missing: %+v", got)
	}
	resumed, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-2", "req-2", 1))
	removed, _ := s.AnswerProbe(ctx, ProbeAnswer{RegistrationID: id, ProbeID: resumed.PendingProbe.ID, Revision: resumed.PendingProbe.Revision, ChannelID: "lease-2", OK: false})
	if removed.Code != "" || removed.Removed || removed.Session.Status != StatusMissing || removed.Session.ErrorCode != "identity-mismatch" {
		t.Fatalf("identity mismatch did not retain a missing registration: %+v", removed)
	}
	if got, err := s.GetSession(ctx, "pc-a"); err != nil || got.RegistrationID != id {
		t.Fatalf("mismatched registration disappeared: %+v %v", got, err)
	}
	b := durableRegister(2, "pc-b", "client-bravo-0002", durableSecretB, "lease-b", "req-b")
	s.RegisterSession(ctx, b)
	if err := s.StopSession(ctx, "pc-b"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(70*time.Minute - time.Second)
	if got, err := s.GetSession(ctx, "pc-b"); err != nil || got.ErrorCode != "detached" || got.ProbePending {
		t.Fatalf("detached registration probed or removed early: %+v %v", got, err)
	}
	*now = now.Add(2 * time.Second)
	if _, err := s.GetSession(ctx, "pc-b"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("detached registration never expired: %v", err)
	}
}

func TestDurableRestartRetainsMissingAndQuarantinesCorruptFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, now := clockedService(t, root)
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, _ := s.RegisterSession(ctx, a)
	*now = now.Add(31*time.Second + time.Hour)
	if got, _ := s.GetSession(ctx, "pc-a"); !got.ProbePending {
		t.Fatalf("probe missing: %+v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "BROKEN.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := newRemoteService(root, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	boot := now.Add(3 * time.Hour)
	later := boot
	restarted.now = func() time.Time { return later }
	restarted.bootedAt = boot
	if got, err := restarted.GetSession(ctx, "pc-a"); err != nil || got.RegistrationID != first.Session.RegistrationID {
		t.Fatalf("restart removed a registration whose deadline passed while the server was down: %+v %v", got, err)
	}
	later = boot.Add(restarted.bootGrace - time.Second)
	if _, err := restarted.GetSession(ctx, "pc-a"); err != nil {
		t.Fatalf("removed inside the boot grace: %v", err)
	}
	later = boot.Add(restarted.bootGrace + time.Second)
	if got, err := restarted.GetSession(ctx, "pc-a"); err != nil || got.RegistrationID != first.Session.RegistrationID || got.Status != StatusMissing {
		t.Fatalf("missing registration disappeared after boot grace: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "BROKEN.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt file was not quarantined")
	}
	if quarantined, _ := filepath.Glob(filepath.Join(root, "BROKEN.json.corrupt*")); len(quarantined) != 1 {
		t.Fatalf("quarantine copy missing: %v", quarantined)
	}
}

func TestDurableStorageFailureLeavesNoPartialState(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, t.TempDir())
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	s.store.writeFile = func(string, []byte) error { return errors.New("disk full") }
	if r, err := s.RegisterSession(ctx, a); err != nil || r.Code != CodeStorage {
		t.Fatalf("storage failure not reported: %+v %v", r, err)
	}
	if listed, _ := s.ListSessions(ctx); len(listed) != 0 {
		t.Fatalf("failed registration left in memory: %+v", listed)
	}
	s.store.writeFile = nil
	first, _ := s.RegisterSession(ctx, a)
	s.store.writeFile = func(string, []byte) error { return errors.New("disk full") }
	if r, _ := s.ResumeSession(ctx, resumeFrom(a, first.Session.RegistrationID, "lease-2", "req-2", 1)); r.Code != CodeStorage {
		t.Fatalf("resume storage failure not reported: %+v", r)
	}
	if got, _ := s.GetSession(ctx, "pc-a"); got.Revision != 1 || got.Generation != first.Session.Generation {
		t.Fatalf("failed resume changed state: %+v", got)
	}
	if _, err := s.WaitForIngress(ctx, "pc-a", "lease-2"); err == nil {
		t.Fatal("unpersisted lease became active")
	}
}

func TestDurableAdmissionHookAssignsNameAndBlocksSlotConflicts(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	s.SetAdmission(admissionFunc(func(_ context.Context, r RegisterRequest) (string, string, error) {
		switch r.Token {
		case "slot-a":
			return "reserved-a", "", nil
		case "slot-uuid":
			return "3f2a9c1e-7b4d-4e8a-9c6f-1a2b3c4d5e6f", "", nil
		}
		return "", CodeInvalidToken, nil
	}))
	a := durableRegister(1, "ignored", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	if r, _ := s.RegisterSession(ctx, a); r.Code != CodeInvalidToken {
		t.Fatalf("missing token admitted: %+v", r)
	}
	a.Token = "slot-a"
	first, _ := s.RegisterSession(ctx, a)
	if first.Code != "" || first.Session.Name != "reserved-a" || first.Warning != "" {
		t.Fatalf("admitted name not enforced: %+v", first)
	}
	b := durableRegister(2, "", "client-bravo-0002", durableSecretB, "lease-2", "req-2")
	b.Token = "slot-a"
	if r, _ := s.RegisterSession(ctx, b); r.Code != CodeOwned {
		t.Fatalf("token-mode slot conflict fell back to UUID nickname: %+v", r)
	}
	if listed, _ := s.ListSessions(ctx); len(listed) != 1 {
		t.Fatalf("conflicting registration created: %+v", listed)
	}
	b.Token = "slot-uuid"
	issued, _ := s.RegisterSession(ctx, b)
	if issued.Code != "" || issued.Session.Name != "3f2a9c1e-7b4d-4e8a-9c6f-1a2b3c4d5e6f" {
		t.Fatalf("UI-issued UUID nickname rejected: %+v", issued)
	}
	if got, err := s.GetSession(ctx, issued.Session.Name); err != nil || got.RegistrationID != issued.Session.RegistrationID {
		t.Fatalf("UUID nickname does not route by name: %+v %v", got, err)
	}
	if _, err := s.AttachSession(ctx, remoteIdentity(7)); err == nil {
		t.Fatal("legacy attach bypassed the token-mode admission policy")
	}
	if listed, _ := s.ListSessions(ctx); len(listed) != 2 {
		t.Fatalf("legacy attach created a registration in token mode: %+v", listed)
	}
}

type contextKeyGrant struct{}

type grantAwareAdmission struct{ admissionFunc }

func (g grantAwareAdmission) Reserved(ctx context.Context) bool {
	return ctx.Value(contextKeyGrant{}) != nil
}

func TestDurableAdmissionWithoutGrantKeepsOpenPolicy(t *testing.T) {
	s, _ := clockedService(t, "")
	s.SetAdmission(grantAwareAdmission{admissionFunc(func(context.Context, RegisterRequest) (string, string, error) {
		return "granted-name", "", nil
	})})
	open := context.Background()
	granted := context.WithValue(open, contextKeyGrant{}, "token")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, _ := s.RegisterSession(open, a)
	if first.Code != "" || first.Session.Name != "pc-a" {
		t.Fatalf("open request was not admitted by the requested name: %+v", first)
	}
	b := durableRegister(2, "pc-a", "client-bravo-0002", durableSecretB, "lease-2", "req-2")
	second, _ := s.RegisterSession(open, b)
	if second.Code != "" || !validCanonicalConversationID(second.Session.Name) || second.Warning == "" {
		t.Fatalf("open conflict lost its UUID fallback under an installed admission: %+v", second)
	}
	if _, err := s.AttachSession(open, remoteIdentity(7)); err != nil {
		t.Fatalf("legacy attach blocked for an open request: %v", err)
	}
	c := durableRegister(3, "ignored", "client-charlie-003", durableSecretB, "lease-3", "req-3")
	third, _ := s.RegisterSession(granted, c)
	if third.Code != "" || third.Session.Name != "granted-name" {
		t.Fatalf("granted request ignored the grant nickname: %+v", third)
	}
	if _, err := s.AttachSession(granted, remoteIdentity(8)); err == nil {
		t.Fatal("legacy attach allowed for a token-protected request")
	}
}

func TestLegacyAttachCannotTakeDurableNameAndLegacyTestsStillHold(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	s.RegisterSession(ctx, a)
	legacy := remoteIdentity(2)
	legacy.Name = "pc-a"
	if _, err := s.AttachSession(ctx, legacy); err == nil {
		t.Fatal("legacy attach took a durable name")
	}
	if got, _ := s.GetSession(ctx, "pc-a"); got.RegistrationID == "" {
		t.Fatalf("durable registration replaced: %+v", got)
	}
	other := remoteIdentity(3)
	if _, err := s.AttachSession(ctx, other); err != nil {
		t.Fatalf("legacy attach broken by durable mode: %v", err)
	}
	d := durableRegister(4, other.Name, "client-delta-0004", durableSecretB, "lease-4", "req-4")
	taken, _ := s.RegisterSession(ctx, d)
	if taken.Code != "" || taken.Session.Name == other.Name || !validCanonicalConversationID(taken.Session.Name) {
		t.Fatalf("live legacy name handling: %+v", taken)
	}
}

func TestDurableValidationRejectsMalformedRequests(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	base := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	for name, mutate := range map[string]func(*RegisterRequest){
		"short secret":   func(r *RegisterRequest) { r.Secret = "short" },
		"uuid name":      func(r *RegisterRequest) { r.Name = sessionUUID(9) },
		"missing lease":  func(r *RegisterRequest) { r.ChannelID = "" },
		"missing client": func(r *RegisterRequest) { r.ClientID = "" },
		"missing req":    func(r *RegisterRequest) { r.RequestID = "" },
		"bad harness":    func(r *RegisterRequest) { r.Harness = "gemini" },
		"bad session":    func(r *RegisterRequest) { r.SessionID = "not-a-uuid" },
		"empty name":     func(r *RegisterRequest) { r.Name = "" },
	} {
		r := base
		mutate(&r)
		if got, err := s.RegisterSession(ctx, r); err != nil || got.Code != CodeInvalid {
			t.Fatalf("%s accepted: %+v %v", name, got, err)
		}
	}
	if listed, _ := s.ListSessions(ctx); len(listed) != 0 {
		t.Fatalf("invalid requests created registrations: %+v", listed)
	}
}

type admissionFunc func(context.Context, RegisterRequest) (string, string, error)

func (f admissionFunc) Admit(ctx context.Context, r RegisterRequest) (string, string, error) {
	return f(ctx, r)
}

func TestDurableProbeExpiresEvenWhileLeaseIsLiveAndLateAnswersAreStale(t *testing.T) {
	ctx := context.Background()
	s, now := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, _ := s.RegisterSession(ctx, a)
	id := first.Session.RegistrationID
	*now = now.Add(31*time.Second + time.Hour)
	if got, _ := s.GetSession(ctx, "pc-a"); !got.ProbePending {
		t.Fatalf("probe missing: %+v", got)
	}
	resumed, _ := s.ResumeSession(ctx, resumeFrom(a, id, "lease-2", "req-2", 1))
	if resumed.Code != "" || resumed.PendingProbe == nil {
		t.Fatalf("resume: %+v", resumed)
	}
	// The client resumes and keeps renewing but never answers the probe.
	for i := 0; i < 39; i++ {
		*now = now.Add(15 * time.Second)
		r, _ := s.RenewLease(ctx, RenewRequest{RegistrationID: id, ChannelID: "lease-2"})
		if r.Code != "" || r.PendingProbe == nil {
			t.Fatalf("renew %d: %+v", i, r)
		}
	}
	*now = now.Add(30 * time.Second)
	if got, err := s.GetSession(ctx, "pc-a"); err != nil || got.Status != StatusMissing || got.AutomaticDeliveryReady || got.ErrorCode != "probe-unanswered" {
		t.Fatalf("unanswered probe did not retire the lease while retaining registration: %+v %v", got, err)
	}
	late, _ := s.AnswerProbe(ctx, ProbeAnswer{RegistrationID: id, ProbeID: resumed.PendingProbe.ID, Revision: resumed.PendingProbe.Revision, ChannelID: "lease-2", OK: true})
	if late.Code == "" || late.Removed {
		t.Fatalf("late probe answer accepted: %+v", late)
	}
}

func TestDurableSelectorNamespacesDoNotAlias(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, _ := s.RegisterSession(ctx, a)
	impostor := durableRegister(2, first.Session.RegistrationID, "client-bravo-0002", durableSecretB, "lease-2", "req-2")
	if r, _ := s.RegisterSession(ctx, impostor); r.Code != CodeInvalid {
		t.Fatalf("registration-id shaped nickname accepted: %+v", r)
	}
	if got, err := s.GetSession(ctx, first.Session.RegistrationID); err != nil || got.Name != "pc-a" {
		t.Fatalf("registration id lookup: %+v %v", got, err)
	}
	legacy := remoteIdentity(3)
	legacy.Name = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := s.AttachSession(ctx, legacy); err == nil {
		t.Fatal("legacy attach accepted a registration-id shaped name")
	}
	if got, err := s.GetSession(ctx, a.SessionID); err != nil || got.RegistrationID != first.Session.RegistrationID {
		t.Fatalf("unique session id lookup: %+v %v", got, err)
	}
}

func TestDurableStaleRegisterCannotDisplaceNewerLease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, now := clockedService(t, root)
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-A", "req-A")
	first, _ := s.RegisterSession(ctx, a)
	id := first.Session.RegistrationID
	b := a
	b.ChannelID, b.RequestID, b.ExpectedRevision = "lease-B", "req-B", 1
	if r, _ := s.RegisterSession(ctx, b); r.Code != "" || r.Session.Revision != 2 {
		t.Fatalf("re-register: %+v", r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, _ := clockedService(t, root)
	restarted.now = func() time.Time { return *now }
	c := resumeFrom(a, id, "lease-C", "req-C", 2)
	if r, _ := restarted.ResumeSession(ctx, c); r.Code != "" || r.Session.Revision != 3 {
		t.Fatalf("resume after restart: %+v", r)
	}
	delayed := a
	delayed.ChannelID, delayed.RequestID, delayed.ExpectedRevision = "lease-A", "req-A2", 1
	if r, _ := restarted.RegisterSession(ctx, delayed); r.Code != CodeRevisionMismatch {
		t.Fatalf("delayed stale register displaced the live lease: %+v", r)
	}
	if _, err := restarted.WaitForIngress(ctx, "pc-a", "lease-A"); err == nil {
		t.Fatal("stale lease became active")
	}
	if got, _ := restarted.GetSession(ctx, "pc-a"); got.Revision != 3 || got.Status != StatusAlive {
		t.Fatalf("live registration changed: %+v", got)
	}
	replay := b
	if r, _ := restarted.RegisterSession(ctx, replay); r.Code != CodeRevisionMismatch {
		t.Fatalf("stale replay after restart was applied: %+v", r)
	}
	fresh := a
	fresh.ChannelID, fresh.RequestID, fresh.ExpectedRevision = "lease-D", "req-D", 3
	if r, _ := restarted.RegisterSession(ctx, fresh); r.Code != "" || r.Session.Revision != 4 {
		t.Fatalf("current explicit register rejected: %+v", r)
	}
	gone := durableRegister(5, "pc-e", "client-echo-00005", durableSecretB, "lease-E", "req-E")
	gone.ExpectedRevision = 7
	if r, _ := restarted.RegisterSession(ctx, gone); r.Code != CodeNotFound {
		t.Fatalf("register with stale state for a removed registration created silently: %+v", r)
	}
}

func TestDurableTypedSelectorsDisambiguateNicknameFromSessionUUID(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-a", "req-a")
	first, _ := s.RegisterSession(ctx, a)
	// The UI reserved B's nickname as exactly A's session UUID.
	s.SetAdmission(admissionFunc(func(context.Context, RegisterRequest) (string, string, error) { return a.SessionID, "", nil }))
	b := durableRegister(2, "", "client-bravo-0002", durableSecretB, "lease-b", "req-b")
	second, _ := s.RegisterSession(ctx, b)
	if second.Code != "" || second.Session.Name != a.SessionID {
		t.Fatalf("nickname equal to a session UUID rejected: %+v", second)
	}
	s.SetAdmission(nil)
	if got, _ := s.GetSession(ctx, "session:"+a.SessionID); got.RegistrationID != first.Session.RegistrationID {
		t.Fatalf("session: selector did not pick the session owner: %+v", got)
	}
	if got, _ := s.GetSession(ctx, "name:"+a.SessionID); got.RegistrationID != second.Session.RegistrationID {
		t.Fatalf("name: selector did not pick the nickname holder: %+v", got)
	}
	if got, _ := s.GetSession(ctx, "registration:"+first.Session.RegistrationID); got.Name != "pc-a" {
		t.Fatalf("registration: selector: %+v", got)
	}
	if _, err := s.GetSession(ctx, "name:missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("typed miss resolved: %v", err)
	}
	if _, err := s.SendMessage(ctx, Message{From: "registration:" + first.Session.RegistrationID, To: "session:" + a.SessionID, Body: "to session owner"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.WaitForIngress(ctx, "pc-a", "lease-a"); err != nil || got.Body != "to session owner" || got.From != "pc-a" {
		t.Fatal(got, err)
	}
	if _, err := s.SendMessage(ctx, Message{From: "pc-a", To: "name:" + a.SessionID, Body: "to nickname"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.WaitForIngress(ctx, a.SessionID, "lease-b"); err != nil || got.Body != "to nickname" || got.To != a.SessionID {
		t.Fatal(got, err)
	}
	if err := s.StopSession(ctx, "session:"+a.SessionID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSession(ctx, "registration:"+first.Session.RegistrationID); got.ErrorCode != "detached" {
		t.Fatalf("session: stop hit the wrong registration: %+v", got)
	}
	if got, _ := s.GetSession(ctx, "registration:"+second.Session.RegistrationID); got.Status != StatusAlive {
		t.Fatalf("nickname holder was detached by session: selector: %+v", got)
	}
	typed := durableRegister(3, "name:evil", "client-charlie-003", durableSecretB, "lease-c", "req-c")
	if r, _ := s.RegisterSession(ctx, typed); r.Code != CodeInvalid {
		t.Fatalf("typed-prefix name accepted: %+v", r)
	}
}

type contextKeyOwner struct{}

type gatingAdmission struct{ admissionFunc }

func (gatingAdmission) Reserved(context.Context) bool { return false }

func (gatingAdmission) CheckSession(ctx context.Context, session Session) error {
	if owner, _ := ctx.Value(contextKeyOwner{}).(string); owner != "" && owner != session.Name {
		return errors.New("grant does not cover this registration")
	}
	return nil
}

func TestDurableAuthorizingAdmissionGatesOwnerOperationsAtomically(t *testing.T) {
	s, _ := clockedService(t, "")
	s.SetAdmission(gatingAdmission{admissionFunc(func(context.Context, RegisterRequest) (string, string, error) { return "", CodeInvalidToken, nil })})
	open := context.Background()
	asA := context.WithValue(open, contextKeyOwner{}, "pc-a")
	asB := context.WithValue(open, contextKeyOwner{}, "pc-b")
	a := durableRegister(1, "pc-a", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, err := s.RegisterSession(open, a)
	if err != nil || first.Code != "" {
		t.Fatalf("open register under a gating admission: %+v %v", first, err)
	}
	id := first.Session.RegistrationID
	b := durableRegister(2, "pc-b", "client-bravo-0002", durableSecretB, "lease-b", "req-b")
	s.RegisterSession(open, b)
	if _, err := s.SendMessage(asB, Message{From: "pc-a", To: "pc-b", Body: "forged"}); err == nil {
		t.Fatal("grant for pc-b could send as pc-a")
	}
	if _, err := s.SendMessage(asA, Message{From: "pc-a", To: "pc-b", Body: "legit"}); err != nil {
		t.Fatalf("owner send rejected: %v", err)
	}
	if r, _ := s.RenewLease(asB, RenewRequest{RegistrationID: id, ChannelID: "lease-1"}); r.Code != CodeOwned {
		t.Fatalf("foreign grant renewed a lease: %+v", r)
	}
	if r, _ := s.ResumeSession(asB, resumeFrom(a, id, "lease-2", "req-2", 1)); r.Code != CodeOwned {
		t.Fatalf("foreign grant resumed: %+v", r)
	}
	if r, _ := s.AnswerProbe(asB, ProbeAnswer{RegistrationID: id, ProbeID: "x", Revision: 1, ChannelID: "lease-1", OK: true}); r.Code != CodeOwned {
		t.Fatalf("foreign grant answered a probe: %+v", r)
	}
	if err := s.ReportIngressFailure(asB, id, "lease-1"); err == nil {
		t.Fatal("foreign grant reported an ingress failure")
	}
	if err := s.StopSession(asB, "pc-a"); err == nil {
		t.Fatal("foreign grant detached a registration")
	}
	waitCtx, cancel := context.WithTimeout(asA, 200*time.Millisecond)
	defer cancel()
	if _, err := s.WaitForIngress(waitCtx, "pc-b", "lease-b"); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("foreign wait was not rejected before dequeue: %v", err)
	}
	if got, err := s.WaitForIngress(asB, "pc-b", "lease-b"); err != nil || got.Body != "legit" {
		t.Fatalf("owner wait: %+v %v", got, err)
	}
	if got, _ := s.GetSession(open, "pc-a"); got.Status != StatusAlive || got.Revision != 1 {
		t.Fatalf("rejected operations changed state: %+v", got)
	}
	// A revoked grant replaying the original request id and secret gets nothing.
	if r, _ := s.RegisterSession(asB, a); r.Code != CodeOwned {
		t.Fatalf("foreign grant obtained a replay answer: %+v", r)
	}
	if r, _ := s.RegisterSession(asA, a); r.Code != "" || r.Session.Revision != 1 {
		t.Fatalf("owner replay rejected: %+v", r)
	}
	if err := s.StopSession(asA, "pc-a"); err != nil {
		t.Fatalf("owner detach rejected: %v", err)
	}
	// A new registration is checked as it would exist before it is created.
	c := durableRegister(3, "pc-c", "client-charlie-003", durableSecretB, "lease-c", "req-c")
	if r, _ := s.RegisterSession(asB, c); r.Code != CodeOwned {
		t.Fatalf("grant for pc-b created pc-c: %+v", r)
	}
	if _, err := s.GetSession(open, "pc-c"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("rejected creation left a registration: %v", err)
	}
	asC := context.WithValue(open, contextKeyOwner{}, "pc-c")
	if r, _ := s.RegisterSession(asC, c); r.Code != "" || r.Session.Name != "pc-c" {
		t.Fatalf("matching grant could not create: %+v", r)
	}
}

type bindingAdmission struct {
	calls          []string
	fail           bool
	checkBoundOnly bool
	bound          bool
}

func (b *bindingAdmission) Admit(_ context.Context, r RegisterRequest) (string, string, error) {
	return "granted-" + r.ClientID, "", nil
}

func (b *bindingAdmission) Claim(_ context.Context, r RegisterRequest) (string, error) {
	b.calls = append(b.calls, RegistrationIdentity(r.Harness, r.SessionID, r.ClientID))
	if b.fail {
		return CodeOwned, nil
	}
	b.bound = true
	return "", nil
}

func (b *bindingAdmission) CheckSession(context.Context, Session) error {
	if b.checkBoundOnly && !b.bound {
		return errors.New("grant is not bound to this registration")
	}
	return nil
}

func TestDurableAdmissionBindRunsOnlyAfterOwnershipChecks(t *testing.T) {
	ctx := context.Background()
	s, _ := clockedService(t, "")
	admission := &bindingAdmission{}
	s.SetAdmission(admission)
	a := durableRegister(1, "", "client-alpha-0001", durableSecretA, "lease-1", "req-1")
	first, _ := s.RegisterSession(ctx, a)
	if first.Code != "" || len(admission.calls) != 1 || admission.calls[0] != "claude:"+a.SessionID+":client-alpha-0001" {
		t.Fatalf("bind after create: %+v %v", first, admission.calls)
	}
	wrong := a
	wrong.Secret, wrong.RequestID, wrong.ChannelID, wrong.ExpectedRevision = durableSecretB, "req-2", "lease-2", 1
	if r, _ := s.RegisterSession(ctx, wrong); r.Code != CodeOwned || len(admission.calls) != 1 {
		t.Fatalf("wrong secret reached bind: %+v %v", r, admission.calls)
	}
	stale := a
	stale.RequestID, stale.ChannelID, stale.ExpectedRevision = "req-3", "lease-3", 9
	if r, _ := s.RegisterSession(ctx, stale); r.Code != CodeRevisionMismatch || len(admission.calls) != 1 {
		t.Fatalf("stale revision reached bind: %+v %v", r, admission.calls)
	}
	admission.fail = true
	failing := a
	failing.RequestID, failing.ChannelID, failing.ExpectedRevision = "req-4", "lease-4", 1
	if r, _ := s.RegisterSession(ctx, failing); r.Code != CodeOwned || len(admission.calls) != 2 {
		t.Fatalf("bind failure not reported as owned: %+v %v", r, admission.calls)
	}
	if got, _ := s.GetSession(ctx, first.Session.Name); got.Revision != 1 {
		t.Fatalf("failed bind changed the registration: %+v", got)
	}
	if _, err := s.WaitForIngress(ctx, first.Session.Name, "lease-4"); err == nil {
		t.Fatal("lease from a failed bind became active")
	}
	// An existing open registration moving to token mode: the grant check
	// must not run before Claim had the chance to bind the new token.
	admission.fail = false
	admission.checkBoundOnly = true
	rebinding := a
	rebinding.RequestID, rebinding.ChannelID, rebinding.ExpectedRevision = "req-5", "lease-5", 1
	if r, _ := s.RegisterSession(ctx, rebinding); r.Code != "" || len(admission.calls) != 3 {
		t.Fatalf("explicit register with a fresh token was refused before claim: %+v %v", r, admission.calls)
	}
	admission.bound = false
	if r, _ := s.RegisterSession(ctx, rebinding); r.Code != CodeOwned {
		t.Fatalf("replay bypassed the grant check: %+v", r)
	}
}
