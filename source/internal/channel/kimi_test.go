//go:build darwin || linux

package channel

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const kimiTestSession = "session_11111111-1111-4111-8111-111111111111"

func TestKimiIdentityNormalizationAndFence(t *testing.T) {
	cwd := t.TempDir()
	k := newKimiClient(os.Getpid(), cwd)
	k.key, _ = kimiProcessKey(os.Getpid())
	if _, err := k.accept(kimiHookInput{Event: "SessionStart", SessionID: kimiTestSession, CWD: cwd}); err != nil {
		t.Fatal(err)
	}
	id, err := k.identify(t.Context(), nil)
	if err != nil || id.Harness != "kimi" || id.SessionID != "11111111-1111-4111-8111-111111111111" {
		t.Fatal(id, err)
	}
	called := false
	k.fence = func() { called = true }
	if _, err := k.accept(kimiHookInput{Event: "SessionStart", SessionID: "session_22222222-2222-4222-8222-222222222222", CWD: cwd}); !errors.Is(err, errKimiReconnect) || !called {
		t.Fatal("changed conversation not fenced", err)
	}
	if _, err := k.identify(t.Context(), nil); !errors.Is(err, errKimiReconnect) {
		t.Fatal(err)
	}
	if err := k.queue.deliver(t.Context(), broker.Message{}); !errors.Is(err, errKimiReconnect) {
		t.Fatal("old receiver accepted", err)
	}
}
func TestKimiIdentityRejectsInvalidInputAndReusedPID(t *testing.T) {
	cwd := t.TempDir()
	k := newKimiClient(os.Getpid(), cwd)
	k.key = "wrong-generation"
	for _, in := range []kimiHookInput{{Event: "SessionStart", SessionID: "11111111-1111-4111-8111-111111111111", CWD: cwd}, {Event: "SessionStart", SessionID: kimiTestSession, CWD: "/wrong"}, {Event: "Unknown", SessionID: kimiTestSession, CWD: cwd}} {
		if _, e := k.accept(in); e == nil {
			t.Fatal("invalid hook accepted")
		}
	}
	_, _ = k.accept(kimiHookInput{Event: "SessionStart", SessionID: kimiTestSession, CWD: cwd})
	if _, e := k.identify(t.Context(), nil); !errors.Is(e, errKimiReconnect) {
		t.Fatal("reused PID accepted", e)
	}
}
func TestKimiIdentityWaitIsBounded(t *testing.T) {
	k := newKimiClient(os.Getpid(), t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := k.identify(ctx, nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	start := time.Now()
	if _, e := k.identify(t.Context(), nil); !errors.Is(e, errKimiUnknown) {
		t.Fatal(e)
	}
	if elapsed := time.Since(start); elapsed < 4900*time.Millisecond || elapsed > 6*time.Second {
		t.Fatal("wrong identity deadline", elapsed)
	}
}
func TestKimiCapabilityDiscovery(t *testing.T) {
	for _, supports := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-server", true: "kimi-server"}[supports], func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("public capability discovery carried credential")
				}
				caps := []string{"scoped-session-v1"}
				if supports {
					caps = append(caps, "harness-kimi-v1")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "capabilities": caps})
			}))
			defer s.Close()
			c := newDeviceAuthClient(strings.Replace(s.URL, "http://", "http://", 1)+"/mcp?project=test", "", nil)
			c.policy = nil
			if _, _, e := c.discoverCapabilities(t.Context()); e != nil {
				t.Fatal(e)
			}
			if c.kimiSupported.Load() != supports {
				t.Fatal("wrong Kimi capability")
			}
		})
	}
}
func TestKimiStatusUnsupportedParent(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server, shutdown := newIngressServerWithOptions(ctx, unavailableUpstream{}, nil, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{}, errUnsupportedHarness
	}, nil, durableOptions{tokenMode: true, identityFailure: errUnsupportedHarness})
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close(); _ = ss.Close(); cancel(); shutdown() }()
	out, err := cli.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if err != nil || out.IsError {
		t.Fatal(out, err)
	}
	raw, _ := json.Marshal(out.StructuredContent)
	var status authStatus
	_ = json.Unmarshal(raw, &status)
	if status.Status != "unsupported-harness" {
		t.Fatal(string(raw))
	}
}

func TestKimiCWDMatchesSymlink(t *testing.T) {
	real := t.TempDir()
	link := real + "-link"
	if e := os.Symlink(real, link); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if !kimiCWDMatches(real, link) || kimiCWDMatches(real, t.TempDir()) {
		t.Fatal("working directory identity check")
	}
}
