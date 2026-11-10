package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type codexYieldRequest struct {
	Version   int    `json:"v"`
	Thread    string `json:"thread"`
	Requester int    `json:"requester"`
	Codex     int    `json:"codex"`
}
type codexYieldCallKey struct{}
type codexYieldCall struct {
	req     *mcp.CallToolRequest
	once    sync.Once
	written bool
}

func withCodexYieldCall(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if req == nil || req.Params == nil {
		return ctx
	}
	if _, ok := req.Params.Meta["threadId"]; !ok {
		return ctx
	}
	return context.WithValue(ctx, codexYieldCallKey{}, &codexYieldCall{req: req})
}
func codexYieldPath(root string, pid int, thread string) string {
	return filepath.Join(root, "codex-resume-claims", "yield-"+strconv.Itoa(pid)+"-"+thread+".json")
}
func parseCodexYield(raw []byte) (codexYieldRequest, error) {
	var r codexYieldRequest
	if len(raw) > 1024 {
		return r, errors.New("oversize yield")
	}
	if err := decodeCodexClaimObject(bytes.NewReader(raw), map[string]any{"v": &r.Version, "thread": &r.Thread, "requester": &r.Requester, "codex": &r.Codex}); err != nil {
		return r, err
	}
	_, err := callerThreadID(deviceRequest(r.Thread))
	if err != nil || r.Version != 1 || r.Requester <= 0 || r.Codex <= 0 {
		return r, errors.New("invalid yield")
	}
	return r, nil
}

// The stable process key includes the start incarnation, not a wall-clock estimate.
func codexYieldHost(pid int) (int, string, error) {
	if _, err := codexClaimProcessStart(pid); err != nil {
		return 0, "", err
	}
	before, err := kimiProcessKey(pid)
	if err != nil {
		return 0, "", err
	}
	parent, err := kimiParentPID(pid)
	for n := 0; err == nil && parent > 0 && n < 16; n++ {
		harness, e := processHarness(parent)
		if e != nil {
			return 0, "", e
		}
		if harness == "codex" {
			key, e := kimiProcessKey(parent)
			after, aerr := kimiProcessKey(pid)
			if e != nil || aerr != nil || before != after {
				return 0, "", errors.New("process changed")
			}
			return parent, key, nil
		}
		next, e := kimiParentPID(parent)
		if next == parent {
			break
		}
		parent, err = next, e
	}
	return 0, "", errors.New("Codex ancestor unavailable")
}
func (o durableOptions) inspectCodexYieldHost(pid int) (int, string, error) {
	if o.codexClaim != nil && o.codexClaim.yieldHost != nil {
		return o.codexClaim.yieldHost(pid)
	}
	return codexYieldHost(pid)
}
func (o durableOptions) requestCodexYield(ctx context.Context, req *mcp.CallToolRequest, id broker.AttachRequest) bool {
	if runtime.GOOS == "windows" || o.state == nil || id.Harness != "codex" || req == nil || req.Params == nil || ctx.Err() != nil {
		return false
	}
	raw, present := req.Params.Meta["threadId"]
	if !present || raw != id.SessionID {
		return false
	}
	if _, err := callerThreadID(req); err != nil {
		return false
	}
	write := func() bool {
		host, _, err := o.inspectCodexYieldHost(os.Getpid())
		if err != nil || host != id.PID {
			return false
		}
		data, _ := json.Marshal(codexYieldRequest{Version: 1, Thread: id.SessionID, Requester: os.Getpid(), Codex: host})
		err = publishCodexYield(codexYieldPath(o.state.Dir(), host, id.SessionID), data)
		return err == nil
	}
	if attempt, ok := ctx.Value(codexYieldCallKey{}).(*codexYieldCall); ok && attempt.req == req {
		attempt.once.Do(func() { attempt.written = write() })
		return attempt.written
	}
	return write()
}
func (o durableOptions) loadRegistrationForCall(ctx context.Context, req *mcp.CallToolRequest, id broker.AttachRequest) (*durableRegistration, func(), error) {
	d, unlock, err := o.loadRegistration(id)
	if !errors.Is(err, clientstate.ErrLocked) || !o.requestCodexYield(ctx, req, id) {
		return d, unlock, err
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-deadline.C:
			return nil, nil, err
		case <-tick.C:
			next, release, e := o.loadRegistration(id)
			if !errors.Is(e, clientstate.ErrLocked) {
				return next, release, e
			}
		}
	}
}
func (o durableOptions) requestCodexYieldForHolder(ctx context.Context, id broker.AttachRequest, c *broker.HolderChallenge) {
	attempt, ok := ctx.Value(codexYieldCallKey{}).(*codexYieldCall)
	if !ok || c == nil {
		return
	}
	current := o.currentHolder(ctx)
	if current == nil || current.DeviceID != c.Holder.DeviceID {
		return
	}
	o.requestCodexYield(ctx, attempt.req, id)
}
func codexClaimCanYieldLocked(b *ingressBinding) bool {
	a := b.messageAck
	if b.claimThread == "" || b.claimConfirmed || b.failed || b.retired || !b.started || a == nil || a.lease != b.identity.ChannelID {
		return false
	}
	select {
	case <-a.failed:
		return false
	default:
		return true
	}
}
func (o durableOptions) handleCodexYield(ctx context.Context, b *ingressBinding, attachMu, mu *sync.Mutex, bindings map[string]*ingressBinding, upstream func(*ingressBinding) Upstream) bool {
	b.mu.Lock()
	thread, id := b.claimThread, b.identity
	b.mu.Unlock()
	if o.state == nil || thread == "" || o.codexClaim == nil {
		return false
	}
	path := codexYieldPath(o.state.Dir(), id.PID, thread)

	data, _, err := readCodexYield(path, true)
	if err != nil {
		if !os.IsNotExist(err) {
			removeCodexYield(path)
		}
		return false
	}
	r, err := parseCodexYield(data)
	if err != nil || r.Thread != thread || r.Codex != id.PID {
		return false
	}
	host, key, err := o.inspectCodexYieldHost(r.Requester)
	o.codexClaim.mu.Lock()
	expected := o.codexClaim.hostKey
	o.codexClaim.mu.Unlock()
	if err != nil || host != id.PID || expected == "" || key != expected {
		return false
	}
	attachMu.Lock()
	defer attachMu.Unlock()
	b.mu.Lock()
	eligible := codexClaimCanYieldLocked(b)
	b.mu.Unlock()
	if !eligible {
		return false
	}

	return releaseCodexClaim(ctx, o, mu, bindings, upstream, b) == nil
}
func (o durableOptions) watchCodexYield(ctx context.Context, b *ingressBinding, attachMu, mu *sync.Mutex, bindings map[string]*ingressBinding, upstream func(*ingressBinding) Upstream) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			b.mu.Lock()
			provisional := b.claimThread != "" && !b.claimConfirmed && !b.failed
			active := codexClaimCanYieldLocked(b)
			thread, pid := b.claimThread, b.identity.PID
			b.mu.Unlock()
			if !provisional {
				return
			}
			if active && o.state != nil {
				if _, err := os.Lstat(codexYieldPath(o.state.Dir(), pid, thread)); err == nil && o.handleCodexYield(ctx, b, attachMu, mu, bindings, upstream) {
					return
				}
			}
		}
	}
}
func cleanupCodexYields(root string, now time.Time) {
	entries, err := os.ReadDir(filepath.Join(root, "codex-resume-claims"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "yield-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(root, "codex-resume-claims", entry.Name())
		data, modified, err := readCodexYield(path, false)
		if err != nil {
			removeCodexYield(path)
			continue
		}
		r, err := parseCodexYield(data)
		if err != nil || now.Sub(modified) > time.Hour {
			removeCodexYield(path)
			continue
		}
		if _, err := codexClaimProcessStart(r.Requester); err != nil {
			removeCodexYield(path)
		}
	}
}

// Claim and yield files share exact-key, duplicate-key and trailing-data checks.
func decodeCodexClaimObject(reader io.Reader, fields map[string]any) error {
	data, err := io.ReadAll(io.LimitReader(reader, 1025))
	if err != nil || len(data) > 1024 {
		return errors.New("invalid claim size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid claim object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		target, allowed := fields[name]
		if !ok || seen[name] || !allowed {
			return errors.New("invalid claim key")
		}
		seen[name] = true
		if err := decoder.Decode(target); err != nil {
			return err
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return errors.New("invalid claim object")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || len(seen) != len(fields) {
		return errors.New("invalid claim schema")
	}
	return nil
}
