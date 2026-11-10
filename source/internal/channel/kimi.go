package channel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

var errKimiUnknown = errors.New("kimi-session-unknown: Kimi hooks are not configured or have not run; configure kimi-hook with the same client state directory")
var errKimiReconnect = errors.New("reconnect-required: Kimi conversation changed; reconnect this MCP client before registering the new conversation")
var errKimiServer = errors.New("unsupported-server: this broker does not advertise harness-kimi-v1; upgrade the test server before connecting Kimi")
var kimiSessionPattern = regexp.MustCompile(`^session_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

type kimiHookInput struct {
	Version   int    `json:"version,omitempty"`
	Receipt   string `json:"receipt,omitempty"`
	Event     string `json:"event"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
}
type kimiClient struct {
	receipts          map[string]*kimiReceipt
	helperVersion     int
	mu                sync.Mutex
	pid               int
	key, cwd, session string
	ready             chan struct{}
	problem           error
	queue             *kimiIngress
	fence             func()
	handedOff         func()
	checkServer       func(context.Context) error
}

func newKimiClient(pid int, cwd string) *kimiClient {
	return &kimiClient{pid: pid, cwd: cwd, ready: make(chan struct{}), queue: &kimiIngress{}}
}
func (k *kimiClient) identify(ctx context.Context, _ *mcp.CallToolRequest) (broker.AttachRequest, error) {
	k.mu.Lock()
	problem := k.problem
	k.mu.Unlock()
	if problem != nil {
		return broker.AttachRequest{}, problem
	}
	select {
	case <-k.ready:
	case <-ctx.Done():
		return broker.AttachRequest{}, ctx.Err()
	case <-time.After(5 * time.Second):
		return broker.AttachRequest{}, errKimiUnknown
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.problem != nil {
		return broker.AttachRequest{}, k.problem
	}
	key, err := kimiProcessKey(k.pid)
	if err != nil || key != k.key {
		return broker.AttachRequest{}, errKimiReconnect
	}
	match := kimiSessionPattern.FindStringSubmatch(k.session)
	if match == nil {
		return broker.AttachRequest{}, errKimiUnknown
	}
	return broker.AttachRequest{Harness: "kimi", PID: k.pid, SessionID: match[1], CWD: k.cwd}, nil
}
func (k *kimiClient) statusError() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.problem != nil {
		return k.problem
	}
	if k.session == "" {
		return errKimiUnknown
	}
	return nil
}
func (k *kimiClient) accept(input kimiHookInput) (map[string]any, error) {
	if kimiSessionPattern.FindStringSubmatch(input.SessionID) == nil || !kimiCWDMatches(k.cwd, input.CWD) {
		return nil, errors.New("Kimi hook identity mismatch")
	}
	if input.Version > 2 {
		return nil, errors.New("unsupported Kimi helper protocol")
	}
	if input.Event == "Receipt" {
		return k.confirmReceipt(input)
	}
	switch input.Event {
	case "SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "Interrupt", "PreToolUse", "PostToolUse":
	default:
		return nil, errors.New("unsupported Kimi hook event")
	}
	k.mu.Lock()
	if k.problem != nil {
		err := k.problem
		k.mu.Unlock()
		return nil, err
	}
	if input.Version == 2 {
		k.helperVersion = 2
	}
	if k.session == "" {
		k.session = input.SessionID
		close(k.ready)
	} else if k.session != input.SessionID {
		k.problem = errKimiReconnect
		fence := k.fence
		k.mu.Unlock()
		k.queue.fence()
		if fence != nil {
			fence()
		}
		return nil, errKimiReconnect
	}
	handedOff := k.handedOff
	k.mu.Unlock()
	out, drained, batch := k.queue.hookHandoff(input.Event, input.Version == 2)
	if input.Version == 2 {
		receipt := ""
		if batch != nil {
			receipt = rand.Text()
			k.mu.Lock()
			if k.receipts == nil {
				k.receipts = map[string]*kimiReceipt{}
			}
			k.receipts[receipt] = &kimiReceipt{batch: batch, finished: make(chan struct{})}
			k.mu.Unlock()
		} else if drained && handedOff != nil {
			handedOff()
		}
		return map[string]any{"version": 2, "receipt": receipt, "output": out}, nil
	}
	if drained && handedOff != nil {
		handedOff()
	}
	return out, nil
}
func (k *kimiClient) listen(ctx context.Context, state string) func() {
	path, err := kimiSocketPath(state, k.pid)
	if err != nil {
		k.problem = errors.New("kimi-hook-unavailable: private hook socket directory is unavailable")
		return func() {}
	}
	k.key, err = kimiProcessKey(k.pid)
	if err != nil {
		k.problem = errKimiReconnect
		return func() {}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		k.problem = errors.New("kimi-hook-unavailable: hook socket could not be bound; check for another client or stale socket")
		return func() {}
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = ln.Close()
		k.problem = errors.New("kimi-hook-unavailable: hook socket permissions could not be set")
		return func() {}
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				if !k.peer(conn) {
					return
				}
				raw, e := io.ReadAll(io.LimitReader(conn, (64<<10)+1))
				if e != nil || len(raw) > 64<<10 {
					return
				}
				var in kimiHookInput
				if json.Unmarshal(raw, &in) != nil {
					return
				}
				out, e := k.accept(in)
				if e != nil {
					return
				}
				if json.NewEncoder(conn).Encode(out) != nil {
					if id, _ := out["receipt"].(string); id != "" {
						k.retryReceipt(id)
					}
					return
				}
				if id, _ := out["receipt"].(string); id != "" {
					_ = conn.CloseWrite()
					k.awaitReceipt(ctx, id)
				}
			}()
		}
	}()
	stop := func() { _ = ln.Close(); workers.Wait() }
	go func() { <-ctx.Done(); _ = ln.Close() }()
	return stop
}
func (k *kimiClient) peer(conn *net.UnixConn) bool {
	pid := kimiPeerPID(conn)
	if pid <= 0 {
		return false
	}
	ancestor, key, err := nearestKimi(pid)
	current, e := kimiProcessKey(k.pid)
	return err == nil && e == nil && ancestor == k.pid && key == k.key && current == k.key
}
func nearestKimi(pid int) (int, string, error) {
	for i := 0; i < 16 && pid > 0; i++ {
		before, err := kimiProcessKey(pid)
		if err != nil {
			return 0, "", errUnsupportedHarness
		}
		h, err := processHarness(pid)
		if err != nil {
			return 0, "", errUnsupportedHarness
		}
		after, err := kimiProcessKey(pid)
		if err != nil || after != before {
			return 0, "", errUnsupportedHarness
		}
		if h == "kimi" {
			return pid, after, nil
		}
		if h != "" {
			return 0, "", errUnsupportedHarness
		}
		next, err := kimiParentPID(pid)
		if err != nil || next == pid {
			break
		}
		pid = next
	}
	return 0, "", errUnsupportedHarness
}

// RunKimiHook never receives a broker token. Missing/unavailable IPC fails open.
func RunKimiHook(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	state := ""
	for i := 0; i < len(args); i++ {
		if (args[i] == "--state-dir" || args[i] == "--auth-dir") && i+1 < len(args) {
			i++
			state = args[i]
		} else {
			return errors.New("kimi-hook accepts only --state-dir or --auth-dir")
		}
	}
	if state == "" {
		var err error
		state, err = clientstate.DefaultDir()
		if err != nil {
			return err
		}
	}
	if !filepath.IsAbs(state) {
		return errors.New("kimi-hook state directory must be absolute")
	}
	raw, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return nil
	}
	var payload struct {
		Event     string `json:"hook_event_name"`
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}
	if json.Unmarshal(raw, &payload) != nil || kimiSessionPattern.FindStringSubmatch(payload.SessionID) == nil || !filepath.IsAbs(payload.CWD) {
		return nil
	}
	pid, _, err := nearestKimi(os.Getppid())
	if err != nil {
		return nil
	}
	path, err := kimiSocketPath(state, pid)
	if err != nil {
		return nil
	}
	request := kimiHookInput{Version: 2, Event: payload.Event, SessionID: payload.SessionID, CWD: payload.CWD}
	data, err := exchangeKimiHook(path, request)
	if err != nil {
		return nil
	}
	return writeKimiHookResponse(path, request, data, output)
}

func writeKimiHookResponse(path string, request kimiHookInput, data []byte, output io.Writer) error {
	var err error
	var packet struct {
		Version int             `json:"version"`
		Receipt string          `json:"receipt"`
		Output  json.RawMessage `json:"output"`
	}
	receipt := ""
	if json.Unmarshal(data, &packet) == nil && packet.Version == 2 && len(packet.Output) != 0 {
		data = append(packet.Output, '\n')
		receipt = packet.Receipt
	}
	n, err := output.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if receipt != "" {
		request.Event = "Receipt"
		request.Receipt = receipt
		_, _ = exchangeKimiHook(path, request)
	}
	return nil
}
func kimiIdentityStatus(ctx context.Context, up Upstream, err error) authStatus {
	code, _, _ := strings.Cut(err.Error(), ":")
	return authVersions(ctx, up, authStatus{Status: code, Message: err.Error()}, "kimi")
}
func kimiSocketName(pid int) string { return fmt.Sprintf("%d.sock", pid) }

func kimiCWDMatches(a, b string) bool {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return false
	}
	ai, e := os.Stat(a)
	if e != nil {
		return false
	}
	bi, e := os.Stat(b)
	return e == nil && ai.IsDir() && bi.IsDir() && os.SameFile(ai, bi)
}

type kimiReceipt struct {
	batch    *kimiBatch
	finished chan struct{}
	once     sync.Once
}

func (r *kimiReceipt) finish() { r.once.Do(func() { close(r.finished) }) }
func (k *kimiClient) confirmReceipt(input kimiHookInput) (map[string]any, error) {
	k.mu.Lock()
	if input.Version != 2 || input.SessionID != k.session || k.problem != nil {
		k.mu.Unlock()
		return nil, errors.New("invalid Kimi receipt")
	}
	receipt := k.receipts[input.Receipt]
	delete(k.receipts, input.Receipt)
	handedOff := k.handedOff
	k.mu.Unlock()
	if receipt == nil {
		return map[string]any{}, nil
	}
	defer receipt.finish()
	if err := receipt.batch.done(); err != nil {
		return nil, err
	}
	if handedOff != nil {
		handedOff()
	}
	return map[string]any{}, nil
}
func (k *kimiClient) retryReceipt(id string) {
	k.mu.Lock()
	receipt := k.receipts[id]
	delete(k.receipts, id)
	k.mu.Unlock()
	if receipt != nil {
		receipt.finish()
		k.queue.retry(receipt.batch)
	}
}
func (k *kimiClient) awaitReceipt(ctx context.Context, id string) {
	k.mu.Lock()
	receipt := k.receipts[id]
	k.mu.Unlock()
	if receipt == nil {
		return
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-receipt.finished:
	case <-ctx.Done():
	case <-timer.C:
		k.retryReceipt(id)
	}
}
func (k *kimiClient) nextGeneration() {
	k.queue.nextGeneration()
	k.mu.Lock()
	for _, receipt := range k.receipts {
		receipt.finish()
	}
	clear(k.receipts)
	k.mu.Unlock()
}
func exchangeKimiHook(path string, input kimiHookInput) ([]byte, error) {
	conn, err := net.DialTimeout("unix", path, 300*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	data, _ := json.Marshal(input)
	n, err := conn.Write(data)
	if err != nil {
		return nil, err
	}
	if n != len(data) {
		return nil, io.ErrShortWrite
	}
	if unix, ok := conn.(*net.UnixConn); ok {
		_ = unix.CloseWrite()
	}
	data, err = io.ReadAll(io.LimitReader(conn, (256<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 256<<10 || !json.Valid(data) {
		return nil, errors.New("invalid Kimi hook response")
	}
	return data, nil
}
