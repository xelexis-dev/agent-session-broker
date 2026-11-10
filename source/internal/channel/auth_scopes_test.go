package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type scopeInfoUpstream struct {
	Upstream
	calls *atomic.Int32
}

func (u scopeInfoUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "session_token_info" {
		u.calls.Add(1)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"scopes": []string{"message:send", "session:register"}, "admissionMode": "manual", "expiresAt": "2030-01-01T00:00:00Z"}}, nil
	}
	return u.Upstream.CallTool(ctx, p)
}
func TestDeviceAuthStatusGetsServerScopesWithoutStoredScopes(t *testing.T) {
	var calls atomic.Int32
	f := newDeviceModeFixture(t, newDeviceAuthServer(t), newSwitchableUpstream(t), t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.wrapUpstream = func(_ string, u Upstream) Upstream { return scopeInfoUpstream{u, &calls} }
	})
	f.grant("11111111-1111-4111-8111-111111111111", "asb_s_synthetic", "scope-fixture")
	waitFor(t, "device registration", func() bool { return f.status("")["registered"] == true })
	// Device exchange intentionally stores no scopes, unlike the direct exchange.
	f.mu.Lock()
	saved := f.saved[len(f.saved)-1]
	f.mu.Unlock()
	if len(saved.Credential.Scopes) != 0 {
		t.Fatal("fixture unexpectedly stores scopes")
	}
	before := calls.Load()
	got := f.status("")
	raw, _ := json.Marshal(got["scopes"])
	if string(raw) != `["message:send","session:register"]` || got["scopeSource"] != "server" {
		t.Fatal("auth_status did not obtain the exact server scopes for a device credential")
	}
	if calls.Load()-before != 1 {
		t.Fatal("auth_status must make exactly one scope request")
	}
	details, ok := got["scopeDetails"].([]any)
	if !ok || len(details) != 2 {
		t.Fatal("scope details missing")
	}
	if !reflect.DeepEqual(got["admissionMode"], "manual") || got["credentialExpiresAt"] != time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339) {
		t.Fatal("server credential metadata missing")
	}
	for _, d := range details {
		m := d.(map[string]any)
		if m["label"] == "" || m["description"] == "" {
			t.Fatal("scope explanation missing")
		}
	}
}

type oldScopeServer struct {
	calls  int
	result *mcp.CallToolResult
	err    error
}

func (u *oldScopeServer) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.calls++
	return u.result, u.err
}
func TestAuthScopesFallbackAndBoundedRequests(t *testing.T) {
	for _, stored := range []bool{false, true} {
		for _, kind := range []string{"old-server", "transport-error", "invalid-scopes"} {
			t.Run(fmt.Sprintf("%t/%s", stored, kind), func(t *testing.T) {
				u := &oldScopeServer{result: &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "synthetic-private-error"}}}}
				if kind == "transport-error" {
					u.err = errors.New("synthetic-private-error")
				}
				if kind == "invalid-scopes" {
					u.result = &mcp.CallToolResult{StructuredContent: map[string]any{"scopes": []string{"synthetic-private-error"}, "admissionMode": "manual"}}
				}
				input := authStatus{Status: authAuthorized}
				want := "unknown"
				if stored {
					input.Scopes = []string{access.SessionRegister}
					input.AdmissionMode = "auto"
					want = "stored"
				}
				out := authScopes(t.Context(), u, input)
				if out.ScopeSource != want || out.Status != authAuthorized || u.calls != 1 {
					t.Fatal("scope fallback changed auth status or request bound")
				}
				raw, _ := json.Marshal(out)
				if bytes.Contains(raw, []byte("synthetic-private-error")) {
					t.Fatal("raw server response leaked")
				}
				if stored && (len(out.ScopeDetails) != 1 || out.AdmissionMode != "auto") {
					t.Fatal("stored details not retained")
				}
			})
		}
	}
	out := authScopes(t.Context(), nil, authStatus{Status: authPending})
	if out.ScopeSource != "unknown" {
		t.Fatal("pending scope source")
	}
}
