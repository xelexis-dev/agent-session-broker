package channel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// A launcher claim is provisional: exclusive consumption does not prove which
// thread owns this MCP connection. Native delivery remains pinned to the claim.
type codexResumeClaim struct {
	mu                  sync.Mutex
	attempted, disabled bool
	identity            broker.AttachRequest
	hostKey             string
	yieldHost           func(int) (int, string, error)
}

func (c *codexResumeClaim) identify(ctx context.Context, req *mcp.CallToolRequest, o durableOptions, identify func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) (broker.AttachRequest, error) {
	id, err := identify(ctx, req)
	if req != nil || os.Getenv("CODEX_THREAD_ID") != "" || id.Harness != "codex" || runtime.GOOS == "windows" {
		return id, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return id, err
	}
	if c.identity.SessionID != "" {
		return c.identity, nil
	}
	if c.attempted || id.PID <= 0 {
		return id, err
	}
	c.attempted = true
	processStart := o.claimProcessStart
	if processStart == nil {
		processStart = codexClaimProcessStart
	}
	started, e := processStart(id.PID)
	if e != nil {
		return id, err
	}
	thread, e := consumeCodexClaim(o.state.Dir(), id.PID, started)
	if e != nil {
		fmt.Fprintln(os.Stderr, "codex-resume-claim: ignored invalid launcher claim")
		return id, err
	}
	if thread == "" {
		return id, err
	}
	id.SessionID = thread
	observe := o.observe
	if observe == nil {
		observe = observeBoundAgent
	}
	observed, e := observe(ctx, id)
	if e != nil || observed.Harness != "codex" || observed.SessionID != thread || observed.PID != id.PID {
		return broker.AttachRequest{}, errors.New("Codex launcher claim could not be verified")
	}
	c.hostKey, _ = kimiProcessKey(id.PID)
	c.identity = observed
	return observed, nil
}
func (c *codexResumeClaim) matches(id broker.AttachRequest) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.disabled && c.identity.SessionID != "" && c.identity.SessionID == id.SessionID && c.identity.PID == id.PID
}
func (c *codexResumeClaim) disable() {
	if c != nil {
		c.mu.Lock()
		c.disabled = true
		c.mu.Unlock()
	}
}

func consumeCodexClaim(root string, pid int, started time.Time) (string, error) {
	path := filepath.Join(root, "codex-resume-claims", strconv.Itoa(pid)+".json")
	file, e := openCodexClaim(path)
	if os.IsNotExist(e) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	defer file.Close()
	info, e := file.Stat()
	if e != nil {
		return "", e
	}
	if info.ModTime().Before(started) {
		_ = os.Remove(path)
		return "", nil
	}
	taken := path + ".taken-" + strconv.Itoa(os.Getpid())
	if e = os.Rename(path, taken); os.IsNotExist(e) {
		return "", nil
	} else if e != nil {
		return "", e
	}
	defer os.Remove(taken)
	// Read the opened inode, and reject replacement between open and rename.
	published, e := os.Lstat(taken)
	if e != nil || !os.SameFile(info, published) {
		return "", errors.New("claim changed")
	}
	version := 0
	thread := ""
	if e := decodeCodexClaimObject(file, map[string]any{"v": &version, "thread": &thread}); e != nil {
		return "", e
	}
	if version != 1 {
		return "", errors.New("invalid claim version")
	}
	_, e = callerThreadID(deviceRequest(thread))
	if e != nil {
		return "", e
	}
	return thread, nil
}

// Called under the same serializer as attach, before any first-call shortcuts
// or the single-credential bound identity check. Join before releasing locks.
func confirmCodexClaim(ctx context.Context, req *mcp.CallToolRequest, o durableOptions, mu *sync.Mutex, bindings map[string]*ingressBinding, upstream func(*ingressBinding) Upstream) error {
	if req == nil || req.Params == nil {
		return nil
	}
	if _, present := req.Params.Meta["threadId"]; !present {
		return nil
	}
	thread, err := callerThreadID(req)
	if err != nil {
		return err
	}
	mu.Lock()
	var old *ingressBinding
	for _, b := range bindings {
		if b.claimThread != "" && !b.claimConfirmed {
			old = b
			break
		}
	}
	if old == nil {
		mu.Unlock()
		return nil
	}
	if old.claimThread == thread {
		o.codexClaim.disable()
		old.claimConfirmed = true
		mu.Unlock()
		o.diagnose(old, "codex-claim-confirmed", "", "")
		return nil
	}
	mu.Unlock()
	err = releaseCodexClaim(ctx, o, mu, bindings, upstream, old)
	if err == nil {
		o.diagnose(old, "codex-claim-mismatch", "identity-mismatch", "")
	}
	return err
}

func releaseCodexClaim(ctx context.Context, o durableOptions, mu *sync.Mutex, bindings map[string]*ingressBinding, upstream func(*ingressBinding) Upstream, old *ingressBinding) error {
	mu.Lock()
	if bindings[old.identity.SessionID] != old {
		mu.Unlock()
		return nil
	}
	o.codexClaim.disable()
	target, id := upstream(old), old.identity
	registrationID := ""
	if old.durable != nil {
		registrationID = old.durable.registrationID
	}
	old.failed = true
	old.retired, old.selfStopped = true, false
	if old.cancel != nil {
		old.cancel()
	}
	done := old.done
	mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.New("previous Codex claim receiver is stopping; retry shortly")
		}
	}
	reportIngressFailure(target, id, registrationID)
	mu.Lock()
	defer mu.Unlock()
	if old.unlock != nil {
		old.unlock()
		old.unlock = nil
	}
	closeUpstream(old.upstream)
	old.upstream = nil
	delete(bindings, old.identity.SessionID)
	return nil
}

func claimAuthStatus(b *ingressBinding, out authStatus) authStatus {
	if b == nil {
		return out
	}
	b.mu.Lock()
	router := b.router
	b.mu.Unlock()
	if router != nil {
		out.CodexDelivery = router.deliveryStatus()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ready := out.Registered && receiverReadyLocked(b)
	out.ReceiverReady = &ready
	if b.claimThread != "" {
		confirmed := b.claimConfirmed
		out.IdentitySource = "launcher-claim"
		out.IdentityConfirmed = &confirmed
	}
	return out
}

// Caller holds b.mu. A ready bit from another lease/generation is not evidence.
func receiverReadyLocked(b *ingressBinding) bool {
	if !b.started || b.failed || b.retired || b.receiveUnavailable || b.receiveReason != "" || b.resumeSuspended || b.holderWaiting || b.generation == 0 || !b.ingressReady || b.identity.ChannelID == "" || b.ingressReadyChannel != b.identity.ChannelID || b.ingressReadyGeneration != b.generation || b.done == nil || (b.claimThread != "" && !b.claimConfirmed) {
		return false
	}
	select {
	case <-b.done:
		return false
	default:
		return true
	}
}
