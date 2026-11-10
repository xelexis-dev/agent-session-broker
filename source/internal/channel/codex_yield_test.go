//go:build darwin || linux

package channel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type yieldFixture struct {
	o          durableOptions
	b          *ingressBinding
	mu, attach sync.Mutex
	bindings   map[string]*ingressBinding
	events     []string
	up         *claimReleaseUpstream
	path       string
}

func newYieldFixture(t *testing.T) *yieldFixture {
	t.Helper()
	f := &yieldFixture{}
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.o = durableOptions{state: state, codexClaim: &codexResumeClaim{hostKey: "host-start", yieldHost: func(int) (int, string, error) { return 42, "host-start", nil }}}
	f.up = &claimReleaseUpstream{events: &f.events}
	done := make(chan struct{})
	f.b = &ingressBinding{mu: &f.mu, identity: broker.AttachRequest{Harness: "codex", SessionID: claimUUID, PID: 42, ChannelID: "claim-lease"}, claimThread: claimUUID, started: true, done: done, upstream: f.up, durable: &durableRegistration{registrationID: "SYNTHETIC-U-REGISTRATION"}, messageAck: &messageAckState{lease: "claim-lease", failed: make(chan struct{})}}
	f.b.cancel = func() { f.events = append(f.events, "done"); close(done) }
	f.b.unlock = func() { f.events = append(f.events, "unlock") }
	f.bindings = map[string]*ingressBinding{claimUUID: f.b}
	f.path = codexYieldPath(state.Dir(), 42, claimUUID)
	return f
}
func (f *yieldFixture) write(t *testing.T, r codexYieldRequest) {
	t.Helper()
	raw, _ := json.Marshal(r)
	if err := publishCodexYield(f.path, raw); err != nil {
		t.Fatal(err)
	}
}
func (f *yieldFixture) handle(ctx context.Context) bool {
	return f.o.handleCodexYield(ctx, f.b, &f.attach, &f.mu, f.bindings, func(*ingressBinding) Upstream { return f.up })
}
func validYield() codexYieldRequest {
	return codexYieldRequest{Version: 1, Thread: claimUUID, Requester: 100, Codex: 42}
}

func TestCodexYieldAcceptanceAndGuards(t *testing.T) {
	for _, kind := range []string{"accept", "ack-inactive", "ack-failed", "confirmed", "failed", "other-host", "other-start", "dead-requester", "wrong-codex", "wrong-thread"} {
		t.Run(kind, func(t *testing.T) {
			f := newYieldFixture(t)
			r := validYield()
			switch kind {
			case "ack-inactive":
				f.b.messageAck = nil
			case "ack-failed":
				close(f.b.messageAck.failed)
			case "confirmed":
				f.b.claimConfirmed = true
			case "failed":
				f.b.failed = true
			case "other-host":
				f.o.codexClaim.yieldHost = func(int) (int, string, error) { return 43, "host-start", nil }
			case "other-start":
				f.o.codexClaim.yieldHost = func(int) (int, string, error) { return 42, "new-start", nil }
			case "dead-requester":
				f.o.codexClaim.yieldHost = func(int) (int, string, error) { return 0, "", os.ErrNotExist }
			case "wrong-codex":
				r.Codex = 43
			case "wrong-thread":
				r.Thread = "22222222-2222-4222-8222-222222222222"
			}
			f.write(t, r)
			accepted := f.handle(t.Context())
			if accepted != (kind == "accept") {
				t.Fatalf("accepted=%t", accepted)
			}
			if _, err := os.Lstat(f.path); !os.IsNotExist(err) {
				t.Fatal("request file retained")
			}
			if kind == "accept" {
				if len(f.up.calls) != 1 || strings.Join(f.events, ",") != "done,report,unlock,close" || len(f.bindings) != 0 || !f.o.codexClaim.disabled {
					t.Fatal("yield release order/state")
				}
			} else if len(f.up.calls) != 0 || len(f.events) != 0 || len(f.bindings) != 1 {
				t.Fatal("rejected request changed holder")
			}
		})
	}
}
func TestCodexYieldMalformedFilesAreRemoved(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "0644", "extra", "duplicate", "missing", "oversize", "trailing", "bad-version", "bad-pid"} {
		t.Run(kind, func(t *testing.T) {
			f := newYieldFixture(t)
			r := validYield()
			raw, _ := json.Marshal(r)
			target := ""
			switch kind {
			case "extra":
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"extra":true}`)
			case "duplicate":
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"v":1}`)
			case "missing":
				raw = []byte(`{"v":1}`)
			case "oversize":
				raw = []byte(strings.Repeat("x", 1025))
			case "trailing":
				raw = append(raw, []byte(` {}`)...)
			case "bad-version":
				r.Version = 2
				raw, _ = json.Marshal(r)
			case "bad-pid":
				r.Requester = 0
				raw, _ = json.Marshal(r)
			}
			if err := publishCodexYield(f.path, raw); err != nil {
				t.Fatal(err)
			}
			if kind == "0644" {
				if err := os.Chmod(f.path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" || kind == "hardlink" {
				target = filepath.Join(f.o.state.Dir(), "unchanged-target")
				if err := os.WriteFile(target, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(f.path); err != nil {
					t.Fatal(err)
				}
				link := os.Symlink
				if kind == "hardlink" {
					link = os.Link
				}
				if err := link(target, f.path); err != nil {
					t.Fatal(err)
				}
			}
			if f.handle(t.Context()) || len(f.up.calls) != 0 {
				t.Fatal("malformed request yielded")
			}
			if _, err := os.Lstat(f.path); !os.IsNotExist(err) {
				t.Fatal("malformed request retained")
			}
			if target != "" {
				got, err := os.ReadFile(target)
				if err != nil || string(got) != string(raw) {
					t.Fatal("link target changed")
				}
			}
		})
	}
}
func TestCodexYieldRequestNeedsRealMetadata(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", claimUUID)
	for _, kind := range []string{"autoAttach", "env", "wrong-thread", "claude", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			f := newYieldFixture(t)
			id := f.b.identity
			var req *mcp.CallToolRequest
			if kind != "autoAttach" {
				req = &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}}
			}
			if kind == "wrong-thread" {
				req = deviceRequest("22222222-2222-4222-8222-222222222222")
			}
			if kind == "claude" {
				req = deviceRequest(claimUUID)
				id.Harness = "claude"
			}
			if kind == "metadata" {
				req = deviceRequest(claimUUID)
			}
			ctx := withCodexYieldCall(t.Context(), req)
			written := f.o.requestCodexYield(ctx, req, id)
			if written != (kind == "metadata") {
				t.Fatalf("written=%t", written)
			}
			if written {
				raw, _, err := readCodexYield(f.path, true)
				if err != nil {
					t.Fatal(err)
				}
				r, err := parseCodexYield(raw)
				if err != nil || r.Requester != os.Getpid() || r.Codex != 42 || r.Thread != claimUUID {
					t.Fatal("request identity")
				}
				if !f.o.requestCodexYield(ctx, req, id) {
					t.Fatal("same call lost request outcome")
				}
				if _, err := os.Stat(f.path); !os.IsNotExist(err) {
					t.Fatal("same call republished request")
				}
			} else if _, err := os.Stat(f.path); !os.IsNotExist(err) {
				t.Fatal("non-authoritative request wrote file")
			}
		})
	}
}
func TestCodexYieldLocalLockRetriesWithinCall(t *testing.T) {
	f := newYieldFixture(t)
	id := f.b.identity
	_, unlock, err := f.o.loadRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	drop := sync.OnceFunc(unlock)
	defer drop()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := os.Stat(f.path); err == nil {
				drop()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	_, release, err := f.o.loadRegistrationForCall(ctx, deviceRequest(claimUUID), id)
	if err != nil {
		t.Fatal("same call did not retry lock", err)
	}
	release()
	<-done
}
func TestCodexYieldServerHolderRequiresSameDeviceAndMetadata(t *testing.T) {
	for _, kind := range []string{"same", "other-device", "no-meta"} {
		t.Run(kind, func(t *testing.T) {
			f := newYieldFixture(t)
			f.o.receiverIdentity = fixtureReceiverIdentity{}
			challenge := &broker.HolderChallenge{Holder: *fixtureHolder()}
			if kind == "other-device" {
				challenge.Holder.DeviceID = strings.Repeat("a", 64)
			}
			req := deviceRequest(claimUUID)
			ctx := t.Context()
			if kind != "no-meta" {
				ctx = withCodexYieldCall(ctx, req)
			}
			f.o.resumeClock = devicePollClock{sleep: func(context.Context, time.Duration) error { return context.Canceled }}
			_, err := f.o.waitHolder(ctx, f.b, challenge)
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			_, err = os.Stat(f.path)
			if (err == nil) != (kind == "same") {
				t.Fatal("holder request device/metadata gate")
			}
		})
	}
}
func TestCodexYieldRacesFirstMetaExactlyOnce(t *testing.T) {
	for n := 0; n < 30; n++ {
		f := newYieldFixture(t)
		f.write(t, validYield())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); f.handle(t.Context()) }()
		go func() {
			defer wg.Done()
			f.attach.Lock()
			defer f.attach.Unlock()
			if err := confirmCodexClaim(t.Context(), deviceRequest("22222222-2222-4222-8222-222222222222"), f.o, &f.mu, f.bindings, func(*ingressBinding) Upstream { return f.up }); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		if len(f.up.calls) != 1 || strings.Join(f.events, ",") != "done,report,unlock,close" || len(f.bindings) != 0 {
			t.Fatal("duplicate/missing release")
		}
	}
}
func TestCodexYieldCleanupDeadAndOld(t *testing.T) {
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	paths := map[string]string{}
	for i, kind := range []string{"dead", "old", "live"} {
		r := validYield()
		r.Codex = 42 + i
		r.Requester = os.Getpid()
		if kind == "dead" {
			r.Requester = 1073741823
		}
		raw, _ := json.Marshal(r)
		p := codexYieldPath(state.Dir(), r.Codex, r.Thread)
		if err := publishCodexYield(p, raw); err != nil {
			t.Fatal(err)
		}
		paths[kind] = p
		if kind == "old" {
			if err := os.Chtimes(p, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	claim := filepath.Join(state.Dir(), "codex-resume-claims", "42.json")
	if err := os.WriteFile(claim, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupCodexYields(state.Dir(), now)
	for kind, p := range paths {
		_, err := os.Stat(p)
		if (err == nil) != (kind == "live") {
			t.Fatal("cleanup policy", kind)
		}
	}
	if raw, err := os.ReadFile(claim); err != nil || string(raw) != "unchanged" {
		t.Fatal("startup claim changed")
	}
}
func TestCodexYieldReplaysDequeuedMessage(t *testing.T) {
	up := newSwitchableUpstream(t)
	dir := t.TempDir()
	dequeued := make(chan struct{})
	first := startAckSwapGeneration(t, up, dir, false, func(ctx context.Context, _ broker.Message) error { close(dequeued); <-ctx.Done(); return ctx.Err() })
	first.binding.mu.Lock()
	first.binding.claimThread = first.binding.identity.SessionID
	first.binding.started = true
	first.binding.cancel = first.cancel
	first.binding.done = first.done
	first.binding.unlock = first.unlock
	first.binding.mu.Unlock()
	var sent broker.SendResult
	if err := call(t.Context(), up, "send_message", broker.Message{From: "swap-client", To: "swap-client", Body: "yield-preserved"}, &sent); err != nil || !sent.Accepted {
		first.stop()
		t.Fatal("send failed")
	}
	select {
	case <-dequeued:
	case <-time.After(time.Second):
		first.stop()
		t.Fatal("did not pause before sink")
	}
	o := durableOptions{codexClaim: &codexResumeClaim{}}
	bindings := map[string]*ingressBinding{first.binding.identity.SessionID: first.binding}
	// The real ACK receiver is cancelled/joined by the shared yield release path.
	if err := releaseCodexClaim(t.Context(), o, first.binding.mu, bindings, func(*ingressBinding) Upstream { return up }, first.binding); err != nil {
		t.Fatal(err)
	}
	host := &ackSwapHost{seen: map[string]int{}}
	second := startAckSwapGeneration(t, up, dir, false, func(_ context.Context, m broker.Message) error { host.accept(m); return nil })
	defer second.stop()
	waitFor(t, "yield replay into U", func() bool { n, _ := host.counts(); return n == 1 })
	host.mu.Lock()
	n := host.seen["yield-preserved"]
	host.mu.Unlock()
	if n != 1 {
		t.Fatal("issued message lost/duplicated")
	}
}

func TestCodexYieldVolumeCannotDisturbConfirmedBinding(t *testing.T) {
	f := newYieldFixture(t)
	f.b.claimConfirmed = true
	f.write(t, validYield())
	const entries = 4096
	for i := 0; i < entries; i++ {
		path := codexYieldPath(f.o.state.Dir(), 1000+i, claimUUID)
		if err := os.WriteFile(path, []byte(`{"v":999}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if f.handle(t.Context()) || len(f.events) != 0 || len(f.up.calls) != 0 || len(f.bindings) != 1 {
		t.Fatal("file flood disturbed confirmed holder")
	}
	if _, err := os.Lstat(f.path); !os.IsNotExist(err) {
		t.Fatal("own rejected yield retained")
	}
	files, err := os.ReadDir(filepath.Dir(f.path))
	if err != nil || len(files) != entries {
		t.Fatal("unrelated request files changed")
	}
}
