package clientstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAstraReviewSteerEventRoundTrip(t *testing.T) {
	e := diagnosticFixture(time.Now().UTC())
	e.Event = "steer-disabled-off"
	if !validDiagnostic(e) {
		t.Fatal("writer rejects steer event")
	}
	raw, err := json.Marshal(Diagnostics{Format: 2, Events: []DiagnosticEvent{e}})
	if err != nil {
		t.Fatal(err)
	}
	var out Diagnostics
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 {
		t.Fatal("writer-valid steer event discarded by reader")
	}
}

func TestReviewLegacyPruneDoesNotSplitV2Writers(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := RegistrationKey("review", "codex", "11111111-1111-4111-8111-111111111111")
	e := diagnosticFixture(time.Now().UTC())
	if err = s.RecordDiagnostic(key, e); err != nil {
		t.Fatal(err)
	}
	v2lock := filepath.Join(s.Dir(), "diagnostics", "v2", key+".lock")
	release, err := lockFile(v2lock)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	legacy := filepath.Join(s.Dir(), "diagnostics", key+".json")
	legacyLock := filepath.Join(s.Dir(), "diagnostics", key+".lock")
	if err = os.WriteFile(legacy, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(legacyLock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// 0.17.2 prunes only direct v1 children, including their lock inode.
	if err = os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(legacyLock); err != nil {
		t.Fatal(err)
	}
	e.At = e.At.Add(time.Second)
	if err = s.RecordDiagnostic(key, e); err == nil {
		t.Fatal("v2 writer escaped its held lock after legacy prune")
	}
}

func TestReviewAllDiagnosticEventsSurviveReloadAndOverwrite(t *testing.T) {
	// Cover the complete producer vocabulary, including summary-only renew-success.
	for _, code := range []string{
		"renew-success",
		"renew-failed",
		"receive-failed",
		"sink-failed",
		"receiver-exit",
		"reconnect-start",
		"reconnect-failed",
		"reconnected",
		"auth-transition",
		"transport-created",
		"transport-replaced",
		"probe-failed",
		"activity-unavailable",
		"token-changed",
		"legacy-migration",
		"handover-skipped",
		"follow-message-dropped",
		"codex-stop-missing",
		"kimi-hook-handoff",
		"codex-claim-unconfirmed",
		"codex-claim-confirmed",
		"codex-claim-mismatch",
		"device-identity",
		"steer-uncertain-retired",
		"steer-delivered",
		"steer-rejected",
		"steer-unverified",
		"steer-disabled-off",
		"steer-disabled-unconfirmed-thread",
		"steer-disabled-ack-required",
		"steer-disabled-unsupported-codex",
		"steer-disabled-unverified-host",
	} {
		t.Run(code, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := RegistrationKey("roundtrip", "codex", "11111111-1111-4111-8111-111111111111")
			event := DiagnosticEvent{At: time.Now().UTC(), SessionID: "11111111-1111-4111-8111-111111111111", Event: code}
			if code == "activity-unavailable" {
				event.Reason = "unsupported"
			}
			if code == "device-identity" {
				event.Reason = "secret-insecure"
			}
			if err = store.RecordDiagnostic(key, event); err != nil {
				t.Fatal("write", err)
			}
			for _, phase := range []string{"reload", "overwrite"} {
				reopened, err := Open(store.Dir())
				if err != nil {
					t.Fatal(err)
				}
				if phase == "overwrite" {
					next := event
					next.At = event.At.Add(time.Second)
					next.Event = "transport-replaced"
					next.Reason = ""
					if err = reopened.RecordDiagnostic(key, next); err != nil {
						t.Fatal(err)
					}
				}
				out, err := reopened.LoadDiagnostics(key)
				if err != nil {
					t.Fatal(phase, err)
				}
				found := code == "renew-success" && out.Summary.LastRenewSuccess.Equal(event.At)
				for _, saved := range SafeDiagnostics(out).Events {
					found = found || saved.Event == code && saved.At.Equal(event.At)
				}
				if !found {
					t.Fatal(phase, "lost emitted diagnostic event")
				}
			}
		})
	}
}
