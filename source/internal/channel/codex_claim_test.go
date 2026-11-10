package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestCodexClaimStartupResumesSavedRegistration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("claims are POSIX only")
	}
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint("mismatch=", mismatch), func(t *testing.T) { testCodexClaimStartup(t, mismatch) })
	}
}
func testCodexClaimStartup(t *testing.T, mismatch bool) {
	t.Setenv("CODEX_THREAD_ID", "")
	up := newSwitchableUpstream(t)
	up.service.SetAdmission(grantAdmission{name: "claim-resume"})
	state := t.TempDir()
	id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 4242, CWD: t.TempDir()}
	configure := func(o *durableOptions, identify *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		o.harness = "codex"
		o.receiverIdentity = &diagnosticCauseIdentity{fixtureReceiverIdentity: fixtureReceiverIdentity{unavailable: true}, cause: "secret-insecure"}
		o.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return id, nil }
		o.parentProcessKey = func(context.Context, int) (string, error) {
			return "synthetic-claim-parent", nil
		}
		o.claimProcessStart = func(int) (time.Time, error) { return time.Now().Add(-time.Minute), nil }
		*identify = func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return id, nil }
	}
	first := newDurableFixtureWithPolicy(t, up, state, true, configure)
	first.attach(t, "")
	waitFor(t, "saved Codex registration", func() bool { s, e := up.session("claim-resume"); return e == nil && s.AutomaticDeliveryReady })
	first.stop()
	dir := filepath.Join(state, "codex-resume-claims")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "4242.json"), []byte(`{"v":1,"thread":"`+id.SessionID+`"}`), 0600); e != nil {
		t.Fatal(e)
	}
	var binding *ingressBinding
	next := newDurableFixtureWithPolicy(t, up, state, true, configure, func(o *durableOptions, identify *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		o.bindingCreated = func(b *ingressBinding) {
			if b.identity.SessionID == id.SessionID {
				binding = b
			}
		}
		*identify = func(_ context.Context, r *mcp.CallToolRequest) (broker.AttachRequest, error) {
			thread, e := callerThreadID(r)
			partial := id
			partial.SessionID = thread
			return partial, e
		}
	})
	defer next.stop()
	waitFor(t, "claim resumes before first tools/call", func() bool {
		s, e := up.session("claim-resume")
		return e == nil && s.Revision >= 2 && s.AutomaticDeliveryReady
	})
	if binding == nil {
		t.Fatal("claim binding not created")
	}
	binding.mu.Lock()
	unconfirmed := binding.claimThread == id.SessionID && !binding.claimConfirmed
	binding.mu.Unlock()
	if !unconfirmed {
		t.Fatal("claim was not provisional")
	}
	hasClaimEvent := func(code string) bool {
		out, err := next.state.LoadDiagnostics(binding.durable.key)
		if err != nil {
			t.Fatal("claim diagnostics unavailable")
		}
		for _, event := range out.Events {
			if event.Event == code {
				return true
			}
		}
		return false
	}
	if !hasClaimEvent("codex-claim-unconfirmed") {
		t.Fatal("startup did not record provisional claim")
	}
	provisional := claimAuthStatus(binding, authStatus{})
	if provisional.IdentitySource != "launcher-claim" || provisional.IdentityConfirmed == nil || *provisional.IdentityConfirmed {
		t.Fatal("auth_status did not expose provisional claim")
	}
	before, _ := up.session("claim-resume")
	thread := id.SessionID
	if mismatch {
		thread = "22222222-2222-4222-8222-222222222222"
	}
	var status authStatus
	result, err := next.local.CallTool(t.Context(), &mcp.CallToolParams{Name: "auth_status", Meta: mcp.Meta{"threadId": thread}, Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatal("first meta was rejected before claim reconciliation")
	}
	if !mismatch {
		raw, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(raw, &status)
		if status.IdentitySource != "launcher-claim" || status.IdentityConfirmed == nil || !*status.IdentityConfirmed {
			t.Fatal("claim was not confirmed")
		}
		if status.DeviceIdentity != "secret-insecure" || status.DeviceIdentityHint == "" {
			t.Fatal("claim auth_status lost device identity projection")
		}
		if !hasClaimEvent("codex-claim-confirmed") {
			t.Fatal("first matching metadata did not record claim confirmation")
		}
		after, _ := up.session("claim-resume")
		if after.Revision != before.Revision {
			t.Fatal("confirmation resumed a second time")
		}
	} else {
		binding.mu.Lock()
		failed := binding.failed
		binding.mu.Unlock()
		if !failed {
			t.Fatal("wrong claim not fenced")
		}
		if !hasClaimEvent("codex-claim-mismatch") {
			t.Fatal("first mismatching metadata did not record claim release")
		}
		// Existing saved U can now resume from a separate client; its local lock and broker lease were released.
		third := newDurableFixtureWithPolicy(t, up, state, true, configure)
		defer third.stop()
		waitFor(t, "authoritative U resumes after mismatch", func() bool {
			s, e := up.session("claim-resume")
			return e == nil && s.Revision > before.Revision && s.AutomaticDeliveryReady
		})
	}
}

func claimFile(t *testing.T, root string, pid int, data string) string {
	t.Helper()
	dir := filepath.Join(root, "codex-resume-claims")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, fmt.Sprint(pid)+".json")
	if e := os.WriteFile(path, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

const claimUUID = "11111111-1111-4111-8111-111111111111"
const claimJSON = `{"v":1,"thread":"` + claimUUID + `"}`

func TestCodexClaimValidationAndStalePID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX claims")
	}
	for name, data := range map[string]string{"valid": claimJSON, "version": `{"v":2,"thread":"` + claimUUID + `"}`, "uppercase": `{"v":1,"thread":"AAAAAAAA-1111-4111-8111-111111111111"}`, "extra": `{"v":1,"thread":"` + claimUUID + `","extra":1}`, "duplicate": `{"v":1,"v":1,"thread":"` + claimUUID + `"}`, "large": strings.Repeat(" ", 1025), "trailing": claimJSON + " {}", "missing": `{"thread":"` + claimUUID + `"}`} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			claimFile(t, root, 42, data)
			got, e := consumeCodexClaim(root, 42, time.Now().Add(-time.Minute))
			if name == "valid" {
				if got != claimUUID || e != nil {
					t.Fatal("valid claim refused")
				}
			} else if got != "" || e == nil {
				t.Fatal("invalid claim accepted")
			}
		})
	}
	t.Run("other-pid", func(t *testing.T) {
		root := t.TempDir()
		other := claimFile(t, root, 41, claimJSON)
		got, _ := consumeCodexClaim(root, 42, time.Now().Add(-time.Minute))
		if got != "" {
			t.Fatal("another PID claim consumed")
		}
		if _, err := os.Stat(other); err != nil {
			t.Fatal("another PID claim changed")
		}
	})
	for _, kind := range []string{"stale", "permissions", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			p := claimFile(t, root, 42, claimJSON)
			start := time.Now().Add(-time.Minute)
			switch kind {
			case "stale":
				_ = os.Chtimes(p, start.Add(-time.Minute), start.Add(-time.Minute))
			case "permissions":
				_ = os.Chmod(p, 0644)
			case "symlink":
				_ = os.Rename(p, p+".real")
				if e := os.Symlink(p+".real", p); e != nil {
					t.Fatal(e)
				}
			case "directory":
				_ = os.Remove(p)
				_ = os.Mkdir(p, 0700)
			}
			got, _ := consumeCodexClaim(root, 42, start)
			if got != "" {
				t.Fatal("unsafe claim accepted")
			}
			if kind == "stale" {
				if _, e := os.Stat(p); !os.IsNotExist(e) {
					t.Fatal("stale PID claim retained")
				}
			}
		})
	}
}
func TestCodexClaimConcurrentConsumption(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX claims")
	}
	root := t.TempDir()
	claimFile(t, root, 42, claimJSON)
	var n atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, _ := consumeCodexClaim(root, 42, time.Now().Add(-time.Minute))
			if s != "" {
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("consumers=%d", n.Load())
	}
}
func TestCodexClaimScopeAndEnvironment(t *testing.T) {
	for _, kind := range []string{"env", "nested-pid", "daemon-pid", "claude", "kimi", "tool-meta"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CODEX_THREAD_ID", "")
			root := t.TempDir()
			path := claimFile(t, root, 42, claimJSON)
			state, e := clientstate.Open(root)
			if e != nil {
				t.Fatal(e)
			}
			id := broker.AttachRequest{Harness: "codex", PID: 43}
			var req *mcp.CallToolRequest
			switch kind {
			case "env":
				t.Setenv("CODEX_THREAD_ID", "22222222-2222-4222-8222-222222222222")
			case "claude", "kimi":
				id.Harness = kind
			case "tool-meta":
				req = deviceRequest(claimUUID)
			}
			c := &codexResumeClaim{}
			options := durableOptions{state: state, claimProcessStart: func(int) (time.Time, error) { return time.Now().Add(-time.Minute), nil }, parentProcessKey: func(context.Context, int) (string, error) {
				return "synthetic-claim-parent", nil
			}}
			got, _ := c.identify(t.Context(), req, options, func(_ context.Context, r *mcp.CallToolRequest) (broker.AttachRequest, error) {
				thread, err := callerThreadID(r)
				id.SessionID = thread
				return id, err
			})
			if kind == "env" && got.SessionID != "22222222-2222-4222-8222-222222222222" {
				t.Fatal("environment identity lost priority")
			}
			if kind == "tool-meta" && got.SessionID != claimUUID {
				t.Fatal("tool metadata was not authoritative")
			}
			if _, e := os.Stat(path); e != nil {
				t.Fatal("unrelated claim consumed")
			}
		})
	}
}
func TestCodexClaimWithoutSavedRegistrationNeverAdmits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX claims")
	}
	t.Setenv("CODEX_THREAD_ID", "")
	up := newSwitchableUpstream(t)
	up.service.SetAdmission(grantAdmission{name: "must-not-register"})
	root := t.TempDir()
	claimFile(t, root, 42, claimJSON)
	id := broker.AttachRequest{Harness: "codex", PID: 42, SessionID: claimUUID, CWD: t.TempDir()}
	f := newDurableFixtureWithPolicy(t, up, root, true, func(o *durableOptions, identify *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		o.harness = "codex"
		o.parentProcessKey = func(context.Context, int) (string, error) {
			return "synthetic-claim-parent", nil
		}
		o.claimProcessStart = func(int) (time.Time, error) { return time.Now().Add(-time.Minute), nil }
		o.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return id, nil }
		*identify = func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
			p := id
			p.SessionID = ""
			return p, errors.New("missing startup identity")
		}
	})
	defer f.stop()
	time.Sleep(150 * time.Millisecond)
	if _, e := up.session("must-not-register"); e == nil {
		t.Fatal("claim created a registration")
	}
}

func TestCodexClaimWindowsIgnoresMatchingClaim(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows runtime guard")
	}
	t.Setenv("CODEX_THREAD_ID", "")
	root := t.TempDir()
	path := claimFile(t, root, 42, claimJSON)
	state, e := clientstate.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	c := &codexResumeClaim{}
	_, _ = c.identify(t.Context(), nil, durableOptions{state: state}, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{Harness: "codex", PID: 42}, errors.New("identity missing")
	})
	if c.attempted {
		t.Fatal("Windows attempted claim lookup")
	}
	if _, e = os.Stat(path); e != nil {
		t.Fatal("Windows consumed claim")
	}
}

type claimReleaseUpstream struct {
	calls  []map[string]string
	events *[]string
	closed bool
}

func (u *claimReleaseUpstream) CallTool(_ context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name != "report_ingress_failure" {
		return nil, errors.New("unexpected tool")
	}
	if u.closed {
		return nil, errors.New("upstream closed before release")
	}
	raw, _ := json.Marshal(p.Arguments)
	var args map[string]string
	_ = json.Unmarshal(raw, &args)
	u.calls = append(u.calls, args)
	*u.events = append(*u.events, "report")
	return &mcp.CallToolResult{}, nil
}
func (u *claimReleaseUpstream) Close() error {
	u.closed = true
	*u.events = append(*u.events, "close")
	return nil
}
func TestCodexClaimMismatchReportsLeaseBeforeClose(t *testing.T) {
	for _, kind := range []string{"match", "mismatch", "nil-durable"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			events := []string{}
			up := &claimReleaseUpstream{events: &events}
			done := make(chan struct{})
			id := broker.AttachRequest{Harness: "codex", SessionID: claimUUID, PID: 42, CWD: t.TempDir(), ChannelID: "claim-lease"}
			b := &ingressBinding{mu: &mu, claimThread: claimUUID, identity: id, done: done, upstream: up, durable: &durableRegistration{registrationID: "SYNTHETIC-U-REGISTRATION"}}
			b.cancel = func() { events = append(events, "done"); close(done) }
			b.unlock = func() { events = append(events, "unlock") }
			if kind == "nil-durable" {
				b.durable = nil
			}
			bindings := map[string]*ingressBinding{claimUUID: b}
			thread := claimUUID
			if kind != "match" {
				thread = "22222222-2222-4222-8222-222222222222"
			}
			opts := durableOptions{codexClaim: &codexResumeClaim{}}
			if err := confirmCodexClaim(t.Context(), deviceRequest(thread), opts, &mu, bindings, func(*ingressBinding) Upstream { return up }); err != nil {
				t.Fatal(err)
			}
			if kind == "match" {
				if len(up.calls) != 0 || up.closed || !b.claimConfirmed {
					t.Fatal("match released its own lease")
				}
				return
			}
			if len(up.calls) != 1 {
				t.Fatalf("server lease reports=%d, want 1", len(up.calls))
			}
			got := up.calls[0]
			expectedRegistration := "SYNTHETIC-U-REGISTRATION"
			if kind == "nil-durable" {
				expectedRegistration = ""
			}
			if got["sessionId"] != claimUUID || got["registrationId"] != expectedRegistration || got["channelId"] != "claim-lease" {
				t.Fatal("release did not identify U's exact lease")
			}
			if strings.Join(events, ",") != "done,report,unlock,close" {
				t.Fatal("release must follow receiver join and precede unlock/close")
			}
			if len(bindings) != 0 {
				t.Fatal("wrong U binding retained")
			}
		})
	}
}

func TestCodexClaimDiagnosticsUseV2Only(t *testing.T) {
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sid := "11111111-1111-4111-8111-111111111111"
	key := clientstate.RegistrationKey("claim-v2", "codex", sid)
	legacy := clientstate.DiagnosticEvent{At: time.Now(), SessionID: sid, Event: "auth-transition", AuthState: "authorized"}
	before, err := json.Marshal(clientstate.Diagnostics{Events: []clientstate.DiagnosticEvent{legacy}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state.Dir(), "diagnostics", key+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	b := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Harness: "codex", SessionID: sid}, durable: &durableRegistration{key: key}}
	o := durableOptions{state: state}
	codes := []string{"codex-claim-unconfirmed", "codex-claim-confirmed", "codex-claim-mismatch"}
	for _, event := range codes {
		o.diagnose(b, event, "", "")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("claim diagnose changed v1 bytes")
	}
	merged, err := state.LoadDiagnostics(key)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range merged.Events {
		counts[e.Event]++
	}
	for _, code := range codes {
		if counts[code] != 1 {
			t.Errorf("merged diagnostics missing exact claim event %s", code)
		}
	}
	if counts["auth-transition"] != 1 {
		t.Fatal("legacy event missing from merged load")
	}
	raw, err := os.ReadFile(filepath.Join(state.Dir(), "diagnostics", "v2", key+".json"))
	if err != nil {
		t.Fatal("v2 diagnostics missing")
	}
	var v2 clientstate.Diagnostics
	if json.Unmarshal(raw, &v2) != nil || v2.Format != 2 || len(v2.Events) != len(codes) {
		t.Fatal("claim events were not written exclusively as v2")
	}
}
