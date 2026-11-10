package broker

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func missingFixture(t *testing.T, n int) (*RemoteService, *time.Time, []Session) {
	t.Helper()
	s, now := clockedService(t, t.TempDir())
	rows := []Session{}
	for i := 0; i < n; i++ {
		r := durableRegister(i+1, fmt.Sprint("missing-", i), fmt.Sprint("missing-client-", i), durableSecretA, fmt.Sprint("lease-", i), fmt.Sprint("request-", i))
		r.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
		out, e := s.RegisterSession(t.Context(), r)
		if e != nil || out.Code != "" {
			t.Fatal("register fixture")
		}
		rows = append(rows, out.Session)
	}
	*now = now.Add(time.Hour + 31*time.Second)
	got, e := s.ListSessions(t.Context())
	if e != nil || len(got) != n {
		t.Fatal("probe fixture")
	}
	for _, row := range got {
		if !row.ProbePending {
			t.Fatal("missing initial probe")
		}
	}
	return s, now, rows
}
func missingDetached(s *RemoteService) int {
	n := 0
	for _, e := range s.byRegistration {
		if e.record.Detached {
			n++
		}
	}
	return n
}
func TestMissingExpireLifecycleAndList(t *testing.T) {
	s, now, original := missingFixture(t, 1)
	entry := s.byRegistration[original[0].RegistrationID]
	base := entry.record.LastSeen
	*now = base.Add(29 * 24 * time.Hour)
	rows, e := s.ListSessions(t.Context())
	if e != nil || len(rows) != 1 || rows[0].ErrorCode != "probe-unanswered" || entry.record.Detached {
		t.Fatal("under30d changed registration")
	}
	*now = base.Add(30 * 24 * time.Hour)
	rows, e = s.ListSessions(t.Context())
	if e != nil || len(rows) != 1 || rows[0].ErrorCode != CodeDetached || rows[0].ProbePending || !entry.record.Detached {
		t.Fatal("30d unanswered entry was not detached")
	}
	if entry.record.LastManagement != nil || entry.record.Probe != nil || entry.record.Revision != original[0].Revision {
		t.Fatal("detach invented management/revision or retained probe")
	}
	collision, _ := s.RegisterSession(t.Context(), durableRegister(2, original[0].Name, "other-client", durableSecretB, "other-lease", "other-request"))
	if collision.Code != "" || collision.Session.Name == original[0].Name {
		t.Fatal("detached name was not reserved")
	}
	proofs := []access.DetachedRemovalProof{}
	s.detachedCleanupBatch = func(p []access.DetachedRemovalProof) error { proofs = append(proofs, p...); return nil }
	*now = now.Add(s.probeAfter + s.probeWindow - time.Second)
	if _, e := s.GetSession(t.Context(), original[0].RegistrationID); e != nil {
		t.Fatal("removed before detached interval")
	}
	*now = now.Add(2 * time.Second)
	rows, e = s.ListSessions(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range rows {
		if r.RegistrationID == original[0].RegistrationID {
			t.Fatal("expired detached row still listed")
		}
	}
	if len(proofs) != 1 || !proofs[0].Detached || !proofs[0].ReattachAfterCleanup || proofs[0].RegistrationID != original[0].RegistrationID {
		t.Fatal("missing removal proof")
	}
	claim, _ := s.RegisterSession(t.Context(), durableRegister(3, original[0].Name, "third-client", durableSecretB, "third-lease", "third-request"))
	if claim.Code != "" || claim.Session.Name != original[0].Name {
		t.Fatal("removed registry name not available")
	}
}
func TestMissingExpireBurstBound(t *testing.T) {
	s, now, rows := missingFixture(t, 40)
	*now = s.byRegistration[rows[0].RegistrationID].record.LastSeen.Add(31 * 24 * time.Hour)
	for i := 0; i < 100; i++ {
		s.ListSessions(t.Context())
	}
	if missingDetached(s) != 16 {
		t.Fatalf("burst detached%d, want16", missingDetached(s))
	}
	*now = now.Add(s.sweepInterval - time.Nanosecond)
	s.ListSessions(t.Context())
	if missingDetached(s) != 16 {
		t.Fatal("interval guard omitted")
	}
	*now = now.Add(time.Nanosecond)
	s.ListSessions(t.Context())
	if missingDetached(s) != 32 {
		t.Fatal("second interval did not advance16")
	}
	*now = now.Add(s.sweepInterval)
	s.ListSessions(t.Context())
	if missingDetached(s) != 40 {
		t.Fatal("last interval did not finish")
	}
}
func TestMissingExpireSaveFailureOncePerInterval(t *testing.T) {
	s, now, rows := missingFixture(t, 4)
	*now = s.byRegistration[rows[0].RegistrationID].record.LastSeen.Add(31 * 24 * time.Hour)
	before := map[string]registrationRecord{}
	for id, e := range s.byRegistration {
		before[id] = *e.record
	}
	writes := 0
	s.store.writeFile = func(string, []byte) error { writes++; return errors.New("synthetic save failure") }
	for i := 0; i < 100; i++ {
		s.ListSessions(t.Context())
	}
	if writes != 1 {
		t.Fatalf("failed detach writes%d, want1", writes)
	}
	for id, e := range s.byRegistration {
		if !reflect.DeepEqual(before[id], *e.record) {
			t.Fatal("failed save changed record")
		}
	}
	*now = now.Add(s.sweepInterval)
	s.ListSessions(t.Context())
	if writes != 2 {
		t.Fatal("failure not retried once next interval")
	}
	s.store.writeFile = nil
	*now = now.Add(s.sweepInterval)
	s.ListSessions(t.Context())
	if missingDetached(s) != 4 {
		t.Fatal("repaired store did not detach")
	}
}
func TestMissingExpireSuspendedKept(t *testing.T) {
	s, now, rows := missingFixture(t, 2)
	if s.SetSuspended(t.Context(), rows[1].RegistrationID, true) != nil {
		t.Fatal("suspend")
	}
	*now = s.byRegistration[rows[0].RegistrationID].record.LastSeen.Add(31 * 24 * time.Hour)
	s.ListSessions(t.Context())
	if !s.byRegistration[rows[1].RegistrationID].record.Suspended || s.byRegistration[rows[1].RegistrationID].record.Detached {
		t.Fatal("suspended registration expired")
	}
}
func TestMissingExpireRestartBetweenPhases(t *testing.T) {
	root := t.TempDir()
	s, now := clockedService(t, root)
	req := durableRegister(1, "restart-missing", "restart-client", durableSecretA, "restart-lease", "restart-request")
	first, e := s.RegisterSession(t.Context(), req)
	if e != nil || first.Code != "" {
		t.Fatal("register")
	}
	*now = now.Add(time.Hour + 31*time.Second)
	s.ListSessions(t.Context())
	*now = now.Add(31 * 24 * time.Hour)
	boot := *now
	s.Close()
	restored, later := clockedService(t, root)
	*later = boot
	restored.bootedAt = boot
	restored.lastFlush = boot
	restored.ListSessions(t.Context())
	if missingDetached(restored) != 0 {
		t.Fatal("detached inside restart grace")
	}
	*later = boot.Add(restored.bootGrace - time.Second)
	restored.ListSessions(t.Context())
	if missingDetached(restored) != 0 {
		t.Fatal("early boot grace detach")
	}
	*later = boot.Add(restored.bootGrace + restored.sweepInterval)
	restored.ListSessions(t.Context())
	if missingDetached(restored) != 1 {
		t.Fatal("old probe did not detach after grace")
	}
	at := *later
	restored.Close()
	again, last := clockedService(t, root)
	*last = at.Add(time.Minute)
	again.bootedAt = *last
	again.lastFlush = *last
	*last = at.Add(again.probeAfter + again.probeWindow - time.Second)
	got, e := again.GetSession(t.Context(), first.Session.RegistrationID)
	if e != nil || got.ErrorCode != CodeDetached {
		t.Fatal("detached persistence lost")
	}
	*last = last.Add(2 * time.Second)
	if _, e := again.GetSession(t.Context(), first.Session.RegistrationID); !errors.Is(e, ErrSessionNotFound) {
		t.Fatal("detached record not removed after restart")
	}
}

func TestMissingExpireSQLitePersistence(t *testing.T) {
	s, now, ids, root := lastSeenSQLite(t, 1)
	*now = now.Add(time.Hour + 31*time.Second)
	s.ListSessions(t.Context())
	*now = now.Add(31 * 24 * time.Hour)
	s.ListSessions(t.Context())
	saved := lastSeenStored(t, s, ids[0])
	if !saved.Detached || saved.Probe != nil || saved.LastManagement != nil {
		t.Fatal("SQLite detach not persisted")
	}
	s.Close()
	r, e := newRemoteServiceWithStore(root, false, true)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	r.now = func() time.Time { return *now }
	r.bootedAt = *now
	*now = now.Add(r.probeAfter + r.probeWindow + time.Second)
	if _, e = r.GetSession(t.Context(), ids[0]); !errors.Is(e, ErrSessionNotFound) {
		t.Fatal("SQLite detached row not removed")
	}
}
