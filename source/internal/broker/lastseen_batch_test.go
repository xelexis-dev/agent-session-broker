package broker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestLastSeenRenewDefersInlinePut(t *testing.T) {
	s, now := clockedService(t, t.TempDir())
	s.persistInterval = 10 * time.Second
	ids := make([]string, 3)
	for i := range ids {
		r, err := s.RegisterSession(t.Context(), durableRegister(i+1, "client-"+sessionUUID(i+1), "client-lastseen-0001", durableSecretA, "lease-1", "request-1"))
		if err != nil || r.Code != "" {
			t.Fatal("register", err, r.Code)
		}
		ids[i] = r.Session.RegistrationID
	}
	puts := 0
	s.store.writeFile = func(string, []byte) error { puts++; return nil }
	*now = now.Add(10 * time.Second)
	for _, id := range ids {
		r, err := s.RenewLease(context.Background(), RenewRequest{RegistrationID: id, ChannelID: "lease-1"})
		if err != nil || r.Code != "" {
			t.Fatal("renew", err, r.Code)
		}
		if r.Session.Revision != 1 {
			t.Fatal("LastSeen changed revision")
		}
	}
	if puts != 0 {
		t.Fatalf("renews performed %d inline Puts; want deferred dirty updates", puts)
	}
}

func lastSeenSQLite(t *testing.T, n int) (*RemoteService, *time.Time, []string, string) {
	t.Helper()
	root := t.TempDir()
	s, err := newRemoteServiceWithStore(root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.bootedAt = now
	s.lastFlush = now
	ids := make([]string, n)
	for i := range ids {
		r, err := s.RegisterSession(t.Context(), durableRegister(i+1, "lastseen-"+sessionUUID(i+1), "client-lastseen-0001", durableSecretA, "lease-1", "request-1"))
		if err != nil || r.Code != "" {
			t.Fatal("register", err, r.Code)
		}
		ids[i] = r.Session.RegistrationID
	}
	return s, &now, ids, root
}
func renewLastSeen(t *testing.T, s *RemoteService, ids []string) {
	t.Helper()
	for _, id := range ids {
		r, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: id, ChannelID: "lease-1"})
		if err != nil || r.Code != "" {
			t.Fatal("renew", err, r.Code)
		}
	}
}
func lastSeenStored(t *testing.T, s *RemoteService, id string) registrationRecord {
	t.Helper()
	raw, err := s.store.database.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var r registrationRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestLastSeenOneBatchPerIntervalAndRestart(t *testing.T) {
	s, now, ids, root := lastSeenSQLite(t, 8)
	batches, puts := 0, 0
	s.store.writeFile = func(string, []byte) error { puts++; return nil }
	// Initial renews must never take the legacy Put path.
	*now = now.Add(10 * time.Second)
	renewLastSeen(t, s, ids)
	if puts != 0 {
		t.Fatal("inline Put", puts)
	}
	s.store.writeFile = nil
	s.store.writeBatch = func(v map[string][]byte) error {
		batches++
		if len(v) != len(ids) {
			t.Fatal("not all dirty registrations batched")
		}
		return s.store.database.Batch(v, nil)
	}
	for i := 0; i < 4; i++ {
		*now = now.Add(10 * time.Second)
		renewLastSeen(t, s, ids)
	}
	if batches != 0 {
		t.Fatal("early batch")
	}
	*now = now.Add(10 * time.Second)
	s.sweep()
	if batches != 1 {
		t.Fatalf("batch count=%d want1", batches)
	}
	for _, id := range ids {
		r := lastSeenStored(t, s, id)
		if !r.LastSeen.Equal(now.Add(-10*time.Second)) || r.Revision != 1 || s.byRegistration[id].dirty {
			t.Fatal("checkpoint/revision/dirty mismatch")
		}
	}
	s.sweep()
	if batches != 1 {
		t.Fatal("duplicate interval flush")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := newRemoteServiceWithStore(root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, id := range ids {
		if !restored.byRegistration[id].record.LastSeen.Equal(now.Add(-10 * time.Second)) {
			t.Fatal("restart lost batched LastSeen")
		}
	}
}
func TestLastSeenLifecycleImmediate(t *testing.T) {
	for _, kind := range []string{"rename", "suspend", "detach"} {
		t.Run(kind, func(t *testing.T) {
			s, now, ids, root := lastSeenSQLite(t, 2)
			id := ids[0]
			*now = now.Add(10 * time.Second)
			renewLastSeen(t, s, ids)
			before := lastSeenStored(t, s, id)
			switch kind {
			case "rename":
				_, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: id, ChannelID: "lease-1", Name: "renamed"})
				if err != nil {
					t.Fatal(err)
				}
			case "suspend":
				if err := s.SetSuspended(t.Context(), "registration:"+id, true); err != nil {
					t.Fatal(err)
				}
			case "detach":
				if err := s.StopSession(t.Context(), "registration:"+id); err != nil {
					t.Fatal(err)
				}
			}
			immediate := lastSeenStored(t, s, id)
			if s.byRegistration[id].dirty {
				t.Fatal("immediate persist left dirty")
			}
			if kind == "rename" && immediate.Name != "renamed" || kind == "suspend" && !immediate.Suspended || kind == "detach" && !immediate.Detached {
				t.Fatal("lifecycle not immediately persisted")
			}
			if !immediate.LastSeen.Equal(*now) || before.LastSeen.Equal(immediate.LastSeen) {
				t.Fatal("latest LastSeen missing")
			}
			*now = now.Add(time.Minute)
			s.sweep()
			after := lastSeenStored(t, s, id)
			if !reflect.DeepEqual(after, immediate) {
				t.Fatal("later batch overwrote lifecycle")
			}
			s.Close()
			r, err := newRemoteServiceWithStore(root, false, true)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if !reflect.DeepEqual(*r.byRegistration[id].record, immediate) {
				t.Fatal("restart lost lifecycle")
			}
		})
	}
}
func TestLastSeenBatchFailureRetries(t *testing.T) {
	s, now, ids, _ := lastSeenSQLite(t, 2)
	*now = now.Add(10 * time.Second)
	renewLastSeen(t, s, ids)
	failure := true
	calls := 0
	s.store.writeBatch = func(v map[string][]byte) error {
		calls++
		if failure {
			return errors.New("synthetic disk failure")
		}
		return s.store.database.Batch(v, nil)
	}
	*now = now.Add(50 * time.Second)
	s.sweep()
	for _, id := range ids {
		if !s.byRegistration[id].dirty {
			t.Fatal("failed batch cleared dirty")
		}
	}
	failure = false
	s.sweep()
	if calls != 2 {
		t.Fatal("failed batch not retried")
	}
	for _, id := range ids {
		if s.byRegistration[id].dirty || !lastSeenStored(t, s, id).LastSeen.Equal(now.Add(-50*time.Second)) {
			t.Fatal("retry did not persist")
		}
	}
}
func TestLastSeenCloseAndExpiredRemoval(t *testing.T) {
	t.Run("close", func(t *testing.T) {
		s, now, ids, root := lastSeenSQLite(t, 1)
		*now = now.Add(10 * time.Second)
		renewLastSeen(t, s, ids)
		calls := 0
		s.store.writeBatch = func(v map[string][]byte) error { calls++; return s.store.database.Batch(v, nil) }
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatal("Close did not flush once")
		}
		r, err := newRemoteServiceWithStore(root, false, true)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if !r.byRegistration[ids[0]].record.LastSeen.Equal(*now) {
			t.Fatal("Close lost LastSeen")
		}
	})
	t.Run("detached-expired", func(t *testing.T) {
		s, now, ids, _ := lastSeenSQLite(t, 1)
		*now = now.Add(10 * time.Second)
		renewLastSeen(t, s, ids)
		if err := s.StopSession(t.Context(), "registration:"+ids[0]); err != nil {
			t.Fatal(err)
		}
		// Even a stale dirty flag must never resurrect an entry removed by this sweep.
		s.byRegistration[ids[0]].dirty = true
		calls := 0
		s.store.writeBatch = func(v map[string][]byte) error { calls++; return s.store.database.Batch(v, nil) }
		*now = now.Add(s.probeAfter + s.probeWindow + time.Second)
		s.sweep()
		if s.byRegistration[ids[0]] != nil || calls != 0 {
			t.Fatal("removed entry was re-Put")
		}
		records, err := s.store.database.List()
		if err != nil || len(records) != 0 {
			t.Fatal("expired record restored", err)
		}
	})
}

func TestLastSeenImmediateFailureAndCloseBestEffort(t *testing.T) {
	s, now, ids, _ := lastSeenSQLite(t, 1)
	*now = now.Add(10 * time.Second)
	renewLastSeen(t, s, ids)
	s.store.writeFile = func(string, []byte) error { return errors.New("synthetic failure") }
	if _, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: ids[0], ChannelID: "lease-1", Name: "failed-name"}); err == nil {
		t.Fatal("rename accepted failed persist")
	}
	if !s.byRegistration[ids[0]].dirty {
		t.Fatal("failed immediate persist cleared dirty")
	}
	s.store.writeFile = nil
	s.store.writeBatch = func(map[string][]byte) error { return errors.New("synthetic failure") }
	if err := s.Close(); err != nil {
		t.Fatal("best effort flush changed Close contract", err)
	}
	if !s.byRegistration[ids[0]].dirty {
		t.Fatal("failed Close flush cleared dirty")
	}
}

func TestLastSeenBatchHoldsServiceLock(t *testing.T) {
	s, now, ids, _ := lastSeenSQLite(t, 1)
	*now = now.Add(10 * time.Second)
	renewLastSeen(t, s, ids)
	writes := 0
	s.store.writeBatch = func(v map[string][]byte) error {
		writes++
		if s.mu.TryLock() {
			s.mu.Unlock()
			t.Error("Batch write escaped service lock")
		}
		return s.store.database.Batch(v, nil)
	}
	*now = now.Add(50 * time.Second)
	done := make(chan struct{})
	go func() { defer close(done); s.sweep() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("flush stalled")
	}
	if writes != 1 {
		t.Fatal("missing Batch")
	}
}

func TestLastSeenDeadlineDoesNotSlideOnLifecycle(t *testing.T) {
	s, now, ids, _ := lastSeenSQLite(t, 1)
	*now = now.Add(10 * time.Second)
	renewLastSeen(t, s, ids)
	*now = now.Add(10 * time.Second)
	renewLastSeen(t, s, ids)
	*now = now.Add(10 * time.Second)
	renewLastSeen(t, s, ids)
	*now = now.Add(10 * time.Second)
	renewLastSeen(t, s, ids)
	*now = now.Add(5 * time.Second)
	if _, err := s.RenameSession(t.Context(), RenameRequest{RegistrationID: ids[0], ChannelID: "lease-1", Name: "late-rename"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Second)
	renewLastSeen(t, s, ids)
	calls := 0
	s.store.writeBatch = func(v map[string][]byte) error { calls++; return s.store.database.Batch(v, nil) }
	*now = now.Add(5 * time.Second)
	s.sweep()
	if calls != 1 || !lastSeenStored(t, s, ids[0]).LastSeen.Equal(now.Add(-5*time.Second)) {
		t.Fatal("immediate lifecycle write postponed dirty flush")
	}
}

func TestLastSeenLateFirstRegistrationKeepsSweepDeadline(t *testing.T) {
	s, err := newRemoteServiceWithStore(t.TempDir(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := s.bootedAt
	s.now = func() time.Time { return now }
	now = now.Add(25 * time.Second)
	r, err := s.RegisterSession(t.Context(), durableRegister(1, "late-client", "client-lastseen-0001", durableSecretA, "lease-1", "request-1"))
	if err != nil || r.Code != "" {
		t.Fatal("register", err, r.Code)
	}
	now = now.Add(time.Second)
	renewLastSeen(t, s, []string{r.Session.RegistrationID})
	want := now
	now = s.bootedAt.Add(time.Minute)
	s.sweep()
	if !lastSeenStored(t, s, r.Session.RegistrationID).LastSeen.Equal(want) {
		t.Fatal("first registration postponed the store flush deadline")
	}
}
