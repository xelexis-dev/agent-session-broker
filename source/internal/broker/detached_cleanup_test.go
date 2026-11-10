package broker

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestCleanupCapabilityOverwritesOnRegisterResumeAndHandover(t *testing.T) {
	s, _ := clockedService(t, "")
	s.SetAdmission(&ownerTestAdmission{owner: "owner"})
	ctx := access.WithGrant(t.Context(), access.Grant{OwnerID: "owner", Scopes: []string{access.SessionRegister}})
	req := durableRegister(1, "worker", "cleanup-client", durableSecretA, "first-lease", "first-request")
	req.Harness = "claude"
	req.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
	first, err := s.RegisterSession(ctx, req)
	if err != nil || first.Code != "" {
		t.Fatal("register")
	}
	entry := s.byRegistration[first.Session.RegistrationID]
	if !entry.record.ReattachAfterCleanup {
		t.Fatal("capability not stored")
	}
	req.Capabilities = nil
	if out, err := s.RegisterSession(ctx, req); err != nil || out.Code != "" || entry.record.ReattachAfterCleanup {
		t.Fatal("register replay kept old-holder capability")
	}
	resume := resumeFrom(req, first.Session.RegistrationID, "resume-lease", "resume-request", first.Session.Revision)
	resume.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
	out, err := s.ResumeSession(ctx, resume)
	if err != nil || out.Code != "" || !entry.record.ReattachAfterCleanup {
		t.Fatal("resume capability not stored")
	}
	resume.Capabilities = nil
	if again, err := s.ResumeSession(ctx, resume); err != nil || again.Code != "" || entry.record.ReattachAfterCleanup {
		t.Fatal("resume replay kept capability")
	}
	next := req
	next.SessionID = sessionUUID(2)
	next.ExpectedRevision = out.Session.Revision
	next.RequestID = "handover"
	next.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
	if _, err := s.RebindConversation(ctx, next, first.Session.RegistrationID); err != nil || !entry.record.ReattachAfterCleanup {
		t.Fatal("handover capability not stored", err)
	}
	next.Capabilities = nil
	if _, err := s.RebindConversation(ctx, next, first.Session.RegistrationID); err != nil || entry.record.ReattachAfterCleanup {
		t.Fatal("handover replay did not clear capability", err)
	}
}

func TestDetachedCleanupRequestSweepFailureThenRetry(t *testing.T) {
	s, now := clockedService(t, "")
	calls := 0
	fail := true
	s.detachedCleanupBatch = func(proofs []access.DetachedRemovalProof) error {
		r := proofs[0]
		calls++
		if !r.Detached || !r.ReattachAfterCleanup || r.RegistrationID == "" || r.Revision == 0 || r.Identity == "" {
			t.Error("invalid pre-removal proof")
		}
		if fail {
			return errors.New("synthetic policy failure")
		}
		return nil
	}
	req := durableRegister(1, "worker", "sweep-client", durableSecretA, "sweep-lease", "sweep-register")
	req.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
	first, err := s.RegisterSession(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StopSession(t.Context(), first.Session.RegistrationID); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Hour)
	rows, err := s.ListSessions(t.Context())
	if err != nil || len(rows) != 1 || calls != 1 {
		t.Fatal("request sweep discarded failed cleanup")
	}
	fail = false
	rows, err = s.ListSessions(t.Context())
	if err != nil || len(rows) != 0 || calls != 2 {
		t.Fatal("request sweep did not retry")
	}
}

func TestDetachedCleanupBackgroundSweepUsesConstructorHook(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		s, err := newRemoteServiceWithCleanup("", true, false, func(proofs []access.DetachedRemovalProof) error {
			r := proofs[0]
			calls++
			if !r.Detached {
				t.Error("background proof not detached")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		s.SetProbeSchedule(time.Second, time.Second, 0)
		req := durableRegister(1, "worker", "background-client", durableSecretA, "background-lease", "background-register")
		out, err := s.RegisterSession(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.StopSession(t.Context(), out.Session.RegistrationID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Second)
		synctest.Wait()
		if calls != 1 {
			t.Fatal("background sweep missed constructor-installed hook")
		}
		rows, err := s.ListSessions(t.Context())
		if err != nil || len(rows) != 0 {
			t.Fatal("background cleanup retained record")
		}
	})
}

func TestDetachedCleanupDoesNotRemoveMissingOrSuspended(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		s, now := clockedService(t, "")
		calls := 0
		s.detachedCleanupBatch = func([]access.DetachedRemovalProof) error { calls++; return nil }
		req := durableRegister(1, "worker", "missing-client", durableSecretA, "missing-lease", "missing-register")
		out, err := s.RegisterSession(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if suspended {
			if err := s.SetSuspended(t.Context(), out.Session.RegistrationID, true); err != nil {
				t.Fatal(err)
			}
		}
		*now = now.Add(2 * time.Hour)
		s.sweep()
		*now = now.Add(2 * time.Hour)
		s.sweep()
		if calls != 0 || len(s.byRegistration) != 1 {
			t.Fatal("missing/suspended registration cleaned")
		}
	}
}
