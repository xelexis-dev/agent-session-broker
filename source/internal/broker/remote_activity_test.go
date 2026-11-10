package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func activityRenewJSON(t *testing.T, s *RemoteService, registrationID, lease string, activity any) RegisterResult {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"registrationId": registrationID, "channelId": lease, "activity": activity})
	if err != nil {
		t.Fatal(err)
	}
	var request RenewRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	out, err := s.RenewLease(context.Background(), request)
	if err != nil || out.Code != "" {
		t.Fatalf("renew failed: code=%s err=%v", out.Code, err)
	}
	return out
}

func activityStateJSON(t *testing.T, session Session) string {
	t.Helper()
	raw, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Activity struct {
			State string `json:"state"`
		} `json:"activity"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.Activity.State
}

func TestActivityExpiresWithoutRemovingRegistration(t *testing.T) {
	ctx := context.Background()
	s, now := clockedService(t, "")
	req := durableRegister(1, "activity", "client-activity", durableSecretA, "lease-activity", "register-activity")
	registered, err := s.RegisterSession(ctx, req)
	if err != nil || registered.Code != "" {
		t.Fatal("registration failed")
	}
	result := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, map[string]any{
		"state": "working", "source": "claude-session-file", "sequence": 1, "observedAt": now.Format(time.RFC3339Nano),
	})
	if state := activityStateJSON(t, result.Session); state != "working" {
		t.Fatalf("authorized latest activity=%q, want working", state)
	}
	*now = now.Add(25 * time.Second)
	resultSession, err := s.GetSession(ctx, req.Name)
	if err != nil || activityStateJSON(t, resultSession) != "unknown" {
		t.Fatalf("expired activity must be unknown; err=%v", err)
	}
	list, err := s.ListSessions(ctx)
	if err != nil || len(list) != 1 || list[0].RegistrationID != registered.Session.RegistrationID || list[0].Status != StatusAlive || !list[0].AutomaticDeliveryReady || activityStateJSON(t, list[0]) != "unknown" {
		t.Fatal("activity TTL changed registration or liveness")
	}
}

func activitySample(now time.Time, sequence uint64, state string) map[string]any {
	return map[string]any{"state": state, "source": "claude-session-file", "sequence": sequence, "observedAt": now.Format(time.RFC3339Nano)}
}

func newActivityFixture(t *testing.T) (*RemoteService, *time.Time, RegisterRequest, RegisterResult) {
	t.Helper()
	s, now := clockedService(t, "")
	req := durableRegister(1, "activity", "client-activity", durableSecretA, "lease-activity", "register-activity")
	registered, err := s.RegisterSession(t.Context(), req)
	if err != nil || registered.Code != "" || registered.ActivityVersion != 1 {
		t.Fatal("activity fixture registration failed")
	}
	return s, now, req, registered
}

func TestActivitySequenceAndReceiptFreshness(t *testing.T) {
	s, now, req, registered := newActivityFixture(t)
	first := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 2, "working")).Session.Activity
	if first.Generation != registered.Session.Generation || !first.ReceivedAt.Equal(*now) || !first.ExpiresAt.Equal(now.Add(25*time.Second)) {
		t.Fatal("incorrect server metadata")
	}
	*now = now.Add(10 * time.Second)
	for _, seq := range []uint64{2, 1} {
		got := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, seq, "idle")).Session.Activity
		if got.State != "working" || !got.ReceivedAt.Equal(first.ReceivedAt) || !got.ExpiresAt.Equal(first.ExpiresAt) {
			t.Fatal("duplicate or reordered input refreshed activity")
		}
	}
	*now = now.Add(15 * time.Second)
	expired := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 2, "working")).Session.Activity
	if expired.State != "unknown" || expired.Reason != "observation-expired" {
		t.Fatal("duplicate resurrected expired evidence")
	}
	newer := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 3, "idle")).Session.Activity
	if newer.State != "idle" || newer.Sequence != 3 || !newer.ReceivedAt.Equal(*now) {
		t.Fatal("new sample was not applied")
	}
	// Returned pointers must not grant write access to server state.
	newer.State = "working"
	if current, _ := s.GetSession(t.Context(), req.Name); current.Activity.State != "idle" {
		t.Fatal("query snapshot aliases server state")
	}
}

func TestActivityMalformedAndMissingDoNotBreakHeartbeat(t *testing.T) {
	cases := map[string]func(time.Time) any{
		"missing":      func(time.Time) any { return nil },
		"empty":        func(time.Time) any { return map[string]any{} },
		"array":        func(time.Time) any { return []any{} },
		"string":       func(time.Time) any { return "private-marker" },
		"number":       func(time.Time) any { return 17 },
		"boolean":      func(time.Time) any { return true },
		"bad state":    func(n time.Time) any { v := activitySample(n, 3, "private-marker"); return v },
		"bad source":   func(n time.Time) any { v := activitySample(n, 3, "idle"); v["source"] = "private-marker"; return v },
		"bad reason":   func(n time.Time) any { v := activitySample(n, 3, "idle"); v["reason"] = "private-marker"; return v },
		"extra fields": func(n time.Time) any { v := activitySample(n, 3, "idle"); v["path"] = "private-marker"; return v },
		"oversized": func(n time.Time) any {
			v := activitySample(n, 3, "idle")
			v["reason"] = strings.Repeat("private-marker", 300)
			return v
		},
		"zero sequence":     func(n time.Time) any { return activitySample(n, 0, "idle") },
		"negative sequence": func(n time.Time) any { v := activitySample(n, 3, "idle"); v["sequence"] = -1; return v },
		"fraction sequence": func(n time.Time) any { v := activitySample(n, 3, "idle"); v["sequence"] = 1.5; return v },
		"overflow sequence": func(n time.Time) any {
			v := activitySample(n, 3, "idle")
			v["sequence"] = json.Number("18446744073709551616")
			return v
		},
		"invalid observed time": func(n time.Time) any { v := activitySample(n, 3, "idle"); v["observedAt"] = "private-marker"; return v },
		"absent observed time":  func(n time.Time) any { v := activitySample(n, 3, "idle"); delete(v, "observedAt"); return v },
		"non UTC": func(n time.Time) any {
			v := activitySample(n, 3, "idle")
			v["observedAt"] = n.In(time.FixedZone("offset", 3600)).Format(time.RFC3339)
			return v
		},
		"future time": func(n time.Time) any { return activitySample(n.Add(time.Minute+time.Nanosecond), 3, "idle") },
		"future changed time": func(n time.Time) any {
			v := activitySample(n, 3, "idle")
			v["changedAt"] = n.Add(time.Nanosecond).Format(time.RFC3339Nano)
			return v
		},
		"malformed changed time": func(n time.Time) any { v := activitySample(n, 3, "idle"); v["changedAt"] = "private-marker"; return v },
	}
	for name, makeSample := range cases {
		t.Run(name, func(t *testing.T) {
			s, now, req, registered := newActivityFixture(t)
			activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 2, "working"))
			*now = now.Add(10 * time.Second)
			got := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, makeSample(*now))
			if got.Session.Activity.State != "unknown" || got.Session.Status != StatusAlive || !got.Session.AutomaticDeliveryReady {
				t.Fatal("invalid activity broke heartbeat or retained working")
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(string(raw), "private-marker") {
				t.Fatal("untrusted activity was exposed")
			}
			activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 1, "working"))
			current, _ := s.GetSession(t.Context(), req.Name)
			if current.Activity.State != "unknown" {
				t.Fatal("invalid activity rewound valid sequence boundary")
			}
			*now = now.Add(21 * time.Second) // renewed lease expires at t=40, not original t=30
			current, _ = s.GetSession(t.Context(), req.Name)
			if current.Status != StatusAlive {
				t.Fatal("invalid optional activity failed to renew lease")
			}
		})
	}
}

func TestActivityRejectsStaleSamplesAndChecksGeneration(t *testing.T) {
	s, now, req, registered := newActivityFixture(t)
	first := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 1, "working"))
	*now = now.Add(time.Second)
	wrong := activitySample(*now, 2, "idle")
	wrong["generation"] = registered.Session.Generation + 1
	got := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, wrong)
	if got.Session.Activity.State != "working" || !got.Session.Activity.ReceivedAt.Equal(first.Session.Activity.ReceivedAt) {
		t.Fatal("wrong generation applied activity")
	}
	wrongLease := RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: "wrong-lease"}
	wrongLease.Activity, _ = json.Marshal(activitySample(*now, 2, "idle"))
	if result, _ := s.RenewLease(t.Context(), wrongLease); result.Code != CodeInvalid {
		t.Fatal("wrong lease accepted")
	}
	current, _ := s.GetSession(t.Context(), req.Name)
	if current.Activity.State != "working" {
		t.Fatal("wrong lease modified current activity")
	}
	got = activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(now.Add(-26*time.Second), 3, "idle"))
	if got.Session.Activity.State != "unknown" || got.Session.Activity.Reason != "stale-evidence" {
		t.Fatal("old observation accepted with newer sequence")
	}
	s, now, req, registered = newActivityFixture(t)
	activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 1, "working"))
	*now = now.Add(time.Second)
	got = activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(now.Add(-2*time.Second), 2, "idle"))
	if got.Session.Activity.State != "unknown" || got.Session.Activity.Reason != "stale-evidence" {
		t.Fatal("observedAt moved backward")
	}
}

func TestActivityFutureToleranceUsesReceiptTTL(t *testing.T) {
	s, now, req, registered := newActivityFixture(t)
	got := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(now.Add(time.Minute), 1, "working"))
	if got.Session.Activity.State != "working" || !got.Session.Activity.ExpiresAt.Equal(now.Add(25*time.Second)) {
		t.Fatal("future tolerance changed receipt-based expiry")
	}
	*now = now.Add(25 * time.Second)
	current, _ := s.GetSession(t.Context(), req.Name)
	if current.Activity.State != "unknown" {
		t.Fatal("future timestamp prolonged activity")
	}
}

func TestActivityBindingResetAndRename(t *testing.T) {
	s, now, req, registered := newActivityFixture(t)
	activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 5, "working"))
	// Exact idempotency replay preserves accepted evidence.
	if replay, err := s.RegisterSession(t.Context(), req); err != nil || replay.Code != "" || replay.Session.Activity.State != "working" {
		t.Fatal("registration replay reset activity")
	}
	renamed, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID, Name: "renamed-activity"})
	if err != nil || renamed.Activity.State != "working" || renamed.Activity.Sequence != 5 {
		t.Fatal("rename discarded same-binding activity")
	}
	req.Name = "renamed-activity"
	req.RequestID = "reregister"
	req.ExpectedRevision = renamed.Revision
	again, err := s.RegisterSession(t.Context(), req)
	if err != nil || again.Code != "" || again.Session.Activity.State != "unknown" {
		t.Fatal("explicit re-register retained activity")
	}
	// A re-register on the same lease clears evidence but cannot admit an old sequence.
	got := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 5, "working"))
	if got.Session.Activity.State != "unknown" {
		t.Fatal("same-lease re-register replayed old evidence")
	}
	got = activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 6, "working"))
	resume := resumeFrom(req, registered.Session.RegistrationID, "replacement-activity", "resume-activity", again.Session.Revision)
	resumed, err := s.ResumeSession(t.Context(), resume)
	if err != nil || resumed.Code != "" || resumed.ActivityVersion != 1 || resumed.Session.Activity.State != "unknown" || resumed.Session.Generation <= got.Session.Generation {
		t.Fatal("new lease did not reset activity")
	}
	old := RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID}
	old.Activity, _ = json.Marshal(activitySample(*now, 7, "working"))
	if out, _ := s.RenewLease(t.Context(), old); out.Code != CodeInvalid {
		t.Fatal("old-generation lease accepted")
	}
	fresh := activityRenewJSON(t, s, registered.Session.RegistrationID, resume.ChannelID, activitySample(*now, 1, "idle"))
	if fresh.Session.Activity.State != "idle" || fresh.Session.Activity.Sequence != 1 {
		t.Fatal("new lease did not reset sequence")
	}
}

func TestActivityInactiveAndLegacyUnknown(t *testing.T) {
	for _, operation := range []string{"missing", "suspend", "detach", "receiver failure", "pending ingress", "probe mismatch", "unanswered probe"} {
		t.Run(operation, func(t *testing.T) {
			s, now, req, registered := newActivityFixture(t)
			activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(*now, 1, "working"))
			switch operation {
			case "missing":
				*now = now.Add(30 * time.Second)
			case "suspend":
				if err := s.SetSuspended(t.Context(), req.Name, true); err != nil {
					t.Fatal(err)
				}
			case "detach":
				if err := s.StopSession(t.Context(), req.Name); err != nil {
					t.Fatal(err)
				}
			case "receiver failure":
				if err := s.ReportIngressFailure(t.Context(), req.Name, req.ChannelID); err != nil {
					t.Fatal(err)
				}
			case "pending ingress":
				resume := resumeFrom(req, registered.Session.RegistrationID, "pending-lease", "pending-resume", registered.Session.Revision)
				resume.IngressPending = true
				if out, err := s.ResumeSession(t.Context(), resume); err != nil || out.Code != "" {
					t.Fatal("pending resume failed")
				}
				raw, _ := json.Marshal(activitySample(*now, 1, "working"))
				if out, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: resume.ChannelID, IngressPending: true, Activity: raw}); err != nil || out.Code != "" || out.Session.AutomaticDeliveryReady {
					t.Fatal("pending renew failed")
				}
			case "probe mismatch", "unanswered probe":
				e := s.byRegistration[registered.Session.RegistrationID]
				e.record.Probe = &probeRecord{ID: "activity-probe", Revision: e.record.Revision, Deadline: now.Add(time.Second)}
				s.bootGrace = 0
				if operation == "probe mismatch" {
					if out, err := s.AnswerProbe(t.Context(), ProbeAnswer{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID, ProbeID: "activity-probe", Revision: e.record.Revision, OK: false}); err != nil || out.Code != "" {
						t.Fatal("probe failed")
					}
				} else {
					*now = now.Add(time.Second)
				}
			}
			current, err := s.GetSession(t.Context(), req.Name)
			if err != nil || current.Activity.State != "unknown" || current.RegistrationID != registered.Session.RegistrationID {
				t.Fatal("inactive activity retained working or removed registration")
			}
		})
	}
	s, _, req, _ := newActivityFixture(t)
	legacy, err := s.AttachSession(t.Context(), AttachRequest{Name: "legacy-activity", Harness: req.Harness, SessionID: sessionUUID(2), PID: 222, CWD: req.CWD, ChannelID: "legacy-lease"})
	if err != nil || legacy.Activity.State != "unknown" {
		t.Fatal("old legacy request incompatible")
	}
}

func TestActivityNeverPersistsAndServerRestartResets(t *testing.T) {
	for _, sqlite := range []bool{false, true} {
		t.Run(fmt.Sprint(sqlite), func(t *testing.T) {
			root := t.TempDir()
			s, err := newRemoteServiceWithStore(root, false, sqlite)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			s.now = func() time.Time { return now }
			req := durableRegister(1, "activity-persistent", "client-activity", durableSecretA, "activity-lease", "activity-register")
			registered, err := s.RegisterSession(t.Context(), req)
			if err != nil || registered.Code != "" {
				t.Fatal("persistent registration failed")
			}
			activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(now, 1, "working"))
			// Force the ordinary heartbeat persistence path after an accepted sample.
			now = now.Add(time.Second)
			s.persistInterval = time.Nanosecond
			activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(now, 2, "idle"))
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, marker := range []string{"\"activity\"", "claude-session-file", "observedAt", "receivedAt", "expiresAt"} {
					if bytes.Contains(raw, []byte(marker)) {
						t.Errorf("activity persisted: %s", marker)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			restored, err := newRemoteServiceWithStore(root, false, sqlite)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			current, err := restored.GetSession(t.Context(), req.Name)
			if err != nil || current.Activity.State != "unknown" || current.Status != StatusMissing || current.RegistrationID != registered.Session.RegistrationID {
				t.Fatal("restart lost registration or restored activity")
			}
			resume := resumeFrom(req, registered.Session.RegistrationID, "restart-lease", "restart-resume", current.Revision)
			resumed, err := restored.ResumeSession(t.Context(), resume)
			if err != nil || resumed.Code != "" || resumed.Session.Activity.State != "unknown" {
				t.Fatal("restart resume retained activity")
			}
		})
	}
}

func TestActivityUnknownObservationAndConcurrentQueries(t *testing.T) {
	s := NewRemoteService()
	defer s.Close()
	req := durableRegister(1, "activity-concurrent", "client-activity", durableSecretA, "activity-concurrent-lease", "activity-concurrent-register")
	registered, err := s.RegisterSession(t.Context(), req)
	if err != nil || registered.Code != "" {
		t.Fatal("registration failed")
	}
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				got, err := s.GetSession(t.Context(), req.Name)
				if err != nil || got.Activity == nil || got.Status != StatusAlive {
					t.Error("concurrent get failed")
					return
				}
				got.Activity.State = "caller-mutation"
				list, err := s.ListSessions(t.Context())
				if err != nil || len(list) != 1 || list[0].Activity.State == "caller-mutation" {
					t.Error("concurrent list or snapshot isolation failed")
					return
				}
			}
		}()
	}
	for seq := uint64(1); seq <= 100; seq++ {
		activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, activitySample(time.Now().UTC(), seq, "working"))
	}
	workers.Wait()
	unknown := map[string]any{"state": "unknown", "sequence": 101, "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "reason": "unsupported"}
	got := activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, unknown)
	if got.Session.Activity.State != "unknown" || got.Session.Activity.Reason != "unsupported" || got.Session.Activity.Sequence != 101 || got.Session.Activity.ReceivedAt.IsZero() {
		t.Fatal("valid unknown observation without a supported source was rejected")
	}
}

func TestHeartbeatActivityAndRuntimeTTL25(t *testing.T) {
	s, now, req, registered := newActivityFixture(t)
	raw, _ := json.Marshal(protocol.RuntimeInfo{Model: "fixture-model", Effort: "high", Sequence: 1, Generation: registered.Session.Generation, ObservedAt: *now})
	activity, _ := json.Marshal(activitySample(*now, 1, "working"))
	out, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID, Activity: activity, RuntimeInfo: raw})
	if err != nil || out.Code != "" {
		t.Fatal("renew")
	}
	*now = now.Add(24 * time.Second)
	current, _ := s.GetSession(t.Context(), req.Name)
	if current.Activity.State != "working" || len(s.SessionRuntimeInfo()) != 1 {
		t.Fatal("metadata expired before25s")
	}
	*now = now.Add(time.Second)
	current, _ = s.GetSession(t.Context(), req.Name)
	if current.Activity.State != "unknown" || len(s.SessionRuntimeInfo()) != 0 || current.Status != StatusAlive {
		t.Fatal("TTL25 changed lease or failed to expire metadata")
	}
}
