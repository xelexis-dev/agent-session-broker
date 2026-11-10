package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// newClientToolServer is the client tool surface without ingress, for tests of
// the shared forwarding, attach, docs and profile code. Product servers are
// built by newIngressServerWithOptions.
func newClientToolServer(upstream Upstream, upstreamTools []*mcp.Tool, identify func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error), version ...string) *mcp.Server {
	options := mcpconfig.ServerOptions()
	options.Instructions = clientInstructions + versionInstructions + docsInstructions
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-session-broker", Version: mcpconfig.Version(version...)}, options)
	addDocsTool(server, mcpconfig.Version(version...))
	addClientToolsWithPolicy(server, upstream, upstreamTools, identify, nil, false, nil, nil, nil, nil)
	addToolProfileMiddleware(server)
	return server
}

func catalogClient(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "catalog-test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestModelCatalogTransportTools(t *testing.T) {
	for _, path := range []string{"forwarded", "snapshot"} {
		for _, harness := range []string{"claude", "codex", "unverified"} {
			t.Run(path+"/"+harness, func(t *testing.T) {
				tools := snapshotTools()
				if path == "forwarded" {
					tools = append(tools, &mcp.Tool{Name: "wait_for_message", InputSchema: map[string]any{"type": "object"}}, &mcp.Tool{Name: "register_session", InputSchema: map[string]any{"type": "object"}}, &mcp.Tool{Name: "renew_lease", InputSchema: map[string]any{"type": "object"}})
				}
				identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
					if harness == "unverified" {
						return broker.AttachRequest{}, errors.New("unavailable")
					}
					return broker.AttachRequest{Harness: harness, SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/fixture"}, nil
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				server, shutdown := newIngressServerWithOptions(ctx, &ingressUpstream{}, tools, identify, nil, durableOptions{harness: harness})
				defer func() { cancel(); shutdown() }()
				cs := catalogClient(t, server)
				listed, err := cs.ListTools(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(listed)
				t.Logf("tools/list %s %s bytes=%d", path, harness, len(data))
				if hasTool(listed.Tools, "wait_for_message") {
					t.Error("model catalog exposes internal wait_for_message")
				}
				if hasTool(listed.Tools, "spawn_session") {
					t.Error("remote catalog exposes unsupported spawn_session")
				}
				if got := hasTool(listed.Tools, "ingress_hook"); got != (harness != "claude") {
					t.Errorf("ingress_hook visible=%v for %s", got, harness)
				}
				if harness != "claude" {
					r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "ingress_hook", Arguments: map[string]string{"session_id": "not-bound", "hook_event_name": "Stop"}})
					if err != nil || r.IsError {
						t.Fatal("Codex-compatible hook no longer callable")
					}
				}
			})
		}
	}
}

func TestConfiguredSessionCredentialGuidance(t *testing.T) {
	for _, token := range []string{"asb_s_synthetic", "synthetic-legacy"} {
		t.Run(token[:3], func(t *testing.T) {
			// A configured credential cannot obtain another code through attach_session
			// until its configuration is replaced or changed to a project token.
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/transport-policy" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"project":"test","allow_http":true}`))
					return
				}
				http.Error(w, "private-upstream-detail", http.StatusUnauthorized)
			}))
			defer upstream.Close()
			msg := configuredRejection(t, upstream.URL, token)
			if strings.Contains(msg, "private-upstream-detail") || strings.Contains(msg, token) || strings.Contains(msg, upstream.URL) {
				t.Fatal("raw credential failure leaked")
			}
			if strings.Contains(msg, "legacy single-session") {
				t.Error("modern configured credential mislabeled legacy")
			}
			for _, word := range []string{"replace", "project", "configuration", "reconnect"} {
				if !strings.Contains(strings.ToLower(msg), word) {
					t.Errorf("missing feasible recovery step %s", word)
				}
			}
		})
	}
}

func TestFreshConfiguredSessionAuthStatusRequiresExplicitAttach(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111"}, nil
	}
	server, _ := newIngressServerWithOptions(ctx, &ingressUpstream{}, nil, identify, nil, durableOptions{tokenMode: true})
	r, err := catalogClient(t, server).CallTool(t.Context(), &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if err != nil || r.IsError {
		t.Fatal("auth_status failed")
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "legacy") || !strings.Contains(string(data), conversationNotRegistered) {
		t.Errorf("configured credential label wrong: %s", data)
	}
}

func TestClientInstructionsKeepBrokerReplyRouting(t *testing.T) {
	if !strings.Contains(clientInstructions, "Reply to broker messages only with x-broker send_message using incoming fromRegistrationId as expectRegistrationId, never native SendMessage.") {
		t.Fatal("broker reply routing instruction missing")
	}
}
