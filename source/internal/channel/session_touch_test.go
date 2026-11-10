package channel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestSessionTouchSelectedSourceAndPrune(t *testing.T) {
	for _, pruneFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh-before-prune", true: "pruned-before-prepare"}[pruneFirst], func(t *testing.T) {
			root := t.TempDir()
			store, err := clientstate.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			old := now.Add(-31 * 24 * time.Hour)
			source := clientstate.SessionState{Scope: "synthetic-scope", Harness: "claude", ClientID: "synthetic-client", SessionID: "11111111-1111-4111-8111-111111111111", LastAdmissionMode: "manual", Registration: clientstate.Registration{Secret: "synthetic-proof", RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Revision: 1, Name: "selected"}}
			key := clientstate.SessionKey(source.Scope, source.Harness, source.SessionID, source.ClientID)
			if err := store.SaveSession(key, source); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "sessions", key, "conn.json")
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			// Selection discovery must not itself refresh the abandoned source.
			candidates, err := store.HandoverCandidates(source.Scope, source.Harness, source.ClientID)
			if err != nil || len(candidates) != 1 {
				t.Fatal("candidate")
			}
			id := broker.AttachRequest{Harness: "claude", SessionID: "22222222-2222-4222-8222-222222222222"}
			destKey := clientstate.SessionKey(source.Scope, id.Harness, id.SessionID, source.ClientID)
			initial := clientstate.SessionState{Registration: clientstate.Registration{Secret: "destination-proof"}}
			a := &sessionAuth{key: destKey, state: initial}
			b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{key: destKey, clientID: source.ClientID, session: a}}
			opts := durableOptions{state: store, scope: source.Scope, device: &deviceAuthOptions{}}
			ctx := context.WithValue(context.WithValue(t.Context(), takeoverContextKey{}, "selected"), manualExplicitAttachKey{}, true)
			if pruneFirst {
				result, err := store.Prune(now)
				if err != nil || result.Sessions != 1 {
					t.Fatal("abandoned source not pruned")
				}
				err = opts.prepareConversationHandover(ctx, b, id)
				var refusal broker.HandoverRefusal
				if !errors.As(err, &refusal) || refusal.Reason != "not-found" || !reflect.DeepEqual(a.state, initial) {
					t.Fatal("pruned source did not fail cleanly")
				}
				if _, found, err := store.LoadSession(destKey); err != nil || found {
					t.Fatal("partial handover persisted")
				}
				// Automatic preparation uses the existing no-source fallback.
				if err := opts.prepareConversationHandover(t.Context(), b, id); err != nil {
					t.Fatal("automatic fallback failed")
				}
				return
			}
			opts.device.persist = func(s *clientstate.Store, k string, state clientstate.SessionState) error {
				// Prune runs while preparation is in flight, after the source-use touch.
				done := make(chan error, 1)
				go func() {
					result, err := s.Prune(now)
					if err == nil && result.Sessions != 0 {
						err = errors.New("selected source pruned")
					}
					done <- err
				}()
				if err := <-done; err != nil {
					return err
				}
				return s.SaveSession(k, state)
			}
			if err := opts.prepareConversationHandover(ctx, b, id); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || !info.ModTime().After(old) {
				t.Fatal("selected source was not refreshed")
			}
			if a.state.Handover == nil || a.state.Handover.SourceKey != key {
				t.Fatal("source not actually used")
			}
		})
	}
}

func TestSessionTouchHeartbeatDailyOwnKeyOnly(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "refused"}[refused], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				s, err := clientstate.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				old := now.Add(-31 * 24 * time.Hour)
				own := clientstate.SessionKey("scope", "codex", "own", "client")
				other := clientstate.SessionKey("scope", "codex", "other", "client")
				for _, key := range []string{own, other} {
					if err := s.SaveSession(key, clientstate.SessionState{Registration: clientstate.Registration{Secret: "synthetic-proof"}}); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(filepath.Join(root, "sessions", key, "conn.json"), old, old); err != nil {
						t.Fatal(err)
					}
				}
				modified := func(key string) time.Time {
					info, err := os.Stat(filepath.Join(root, "sessions", key, "conn.json"))
					if err != nil {
						t.Fatal(err)
					}
					return info.ModTime()
				}
				b := heartbeatBinding()
				b.durable.key = own
				b.durable.session = &sessionAuth{}
				u := &heartbeatUpstream{}
				if refused {
					u.renew = func(context.Context, int) (broker.RegisterResult, error) {
						return broker.RegisterResult{Code: broker.CodeSuspended}, nil
					}
				}
				stop := startHeartbeat(t, u, b, durableOptions{state: s, heartbeat: 12 * time.Hour})
				defer stop()
				time.Sleep(12 * time.Hour)
				synctest.Wait()
				first := modified(own)
				if refused {
					if !first.Equal(old) {
						t.Fatal("refused heartbeat touched session")
					}
					return
				}
				if !first.After(old) || !modified(other).Equal(old) {
					t.Fatal("successful heartbeat did not touch only own key")
				}
				time.Sleep(12 * time.Hour)
				synctest.Wait()
				if !modified(own).Equal(first) {
					t.Fatal("session touched twice within24h")
				}
				time.Sleep(12 * time.Hour)
				synctest.Wait()
				if !modified(own).After(first) || !modified(other).Equal(old) {
					t.Fatal("daily refresh missing or touched foreign key")
				}
			})
		})
	}
}
