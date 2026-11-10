package channel

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"sync"
	"testing"
	"time"
)

func TestIngressShutdownFlushesPendingDiagnosticSuccess(t *testing.T) {
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	key := clientstate.RegistrationKey("scope", "codex", id)
	now := time.Now().UTC()
	for i := range 2 {
		if err := state.RecordDiagnostic(key, clientstate.DiagnosticEvent{At: now.Add(time.Duration(i) * time.Millisecond), SessionID: id, Event: "renew-success"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, shutdown := newIngressServerWithOptions(ctx, unavailableUpstream{}, nil, nil, nil, durableOptions{state: state, enabled: true})
	cancel()
	shutdown()
	reopened, _ := clientstate.Open(state.Dir())
	disk, err := reopened.LoadDiagnostics(key)
	if err != nil || !disk.Summary.LastRenewSuccess.Equal(now.Add(time.Millisecond)) {
		t.Fatal("orderly ingress shutdown lost pending success", err)
	}
}

func TestAuthStatusDiagnosticsUsePendingSuccess(t *testing.T) {
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	key := clientstate.RegistrationKey("scope", "codex", id)
	b := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{SessionID: id}, durable: &durableRegistration{key: key}}
	opts := durableOptions{state: state}
	before := time.Now().UTC()
	opts.diagnose(b, "renew-success", "", "")
	first := opts.diagnostics(b).LastRenewSuccess
	opts.diagnose(b, "renew-success", "", "")
	latest := opts.diagnostics(b).LastRenewSuccess
	reopened, _ := clientstate.Open(state.Dir())
	disk, err := reopened.LoadDiagnostics(key)
	if err != nil || first.Before(before) || !latest.After(first) || !disk.Summary.LastRenewSuccess.Equal(first) {
		t.Fatal("auth_status must show memory freshness while file lags", err)
	}
}
