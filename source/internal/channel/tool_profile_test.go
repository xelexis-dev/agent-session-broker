package channel

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestToolProfileOptions(t *testing.T) {
	base := []string{"--broker-url", "https://example.test/mcp?project=demo", "--token", "synthetic-session"}
	for _, value := range []string{"full", "worker"} {
		if _, err := parseClientOptions(append(append([]string{}, base...), "--tool-profile", value)); err != nil {
			t.Fatalf("profile %s rejected", value)
		}
	}
	if _, err := parseClientOptions(append(base, "--tool-profile", "invalid")); err == nil || !strings.Contains(err.Error(), "--tool-profile requires full or worker") {
		t.Fatal("invalid profile needs a clear startup error")
	}
}

type profileUpstream struct{ calls atomic.Int32 }

func (u *profileUpstream) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.calls.Add(1)
	return &mcp.CallToolResult{StructuredContent: map[string]any{"ok": true}}, nil
}

func TestToolProfileEveryClientPath(t *testing.T) {
	for _, path := range []string{"codex-ingress", "claude-ingress", "kimi-ingress", "durable-channel"} {
		for _, profile := range []string{"", "full", "worker"} {
			t.Run(path+"/"+profile, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.WithValue(t.Context(), toolProfileKey{}, profile))
				defer cancel()
				up := &profileUpstream{}
				var tools []*mcp.Tool
				for _, name := range []string{"stop_session", "retire_session", "suspend_session", "unsuspend_session", "set_bug_report_status", "list_bug_reports", "get_bug_report", "report_bug", "memory_put", "memory_get", "memory_search"} {
					tools = append(tools, &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}})
				}
				id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/synthetic"}
				identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return id, nil }
				st, ct := mcp.NewInMemoryTransports()
				done := make(chan struct{})
				switch path {
				case "durable-channel":
					id.Harness = "claude"
					go func() { defer close(done); ServeDurableChannel(ctx, st, up, tools, id, durableOptions{}) }()
				default:
					opts := durableOptions{harness: strings.TrimSuffix(path, "-ingress")}
					if opts.harness == "kimi" {
						opts.kimi = newKimiClient(123, "/synthetic")
					}
					server, shutdown := newIngressServerWithOptions(ctx, up, tools, identify, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
						return func(context.Context, broker.Message) error { return nil }, nil, nil
					}, opts)
					ss, err := server.Connect(ctx, st, nil)
					if err != nil {
						t.Fatal("server connect")
					}
					defer ss.Close()
					defer shutdown()
					close(done)
				}
				// Shutdown joins the serve path after the MCP connection closes.
				defer func() { cancel(); <-done }()
				cli, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic", Version: "1"}, nil).Connect(ctx, ct, nil)
				if err != nil {
					t.Fatal("client connect")
				}
				defer cli.Close()
				listed, err := cli.ListTools(ctx, nil)
				if err != nil {
					t.Fatal("list")
				}
				names := map[string]bool{}
				for _, tool := range listed.Tools {
					names[tool.Name] = true
					if tool.Name == "activate_channel" {
						for _, phrase := range []string{"Internal", "Never call manually", "poll"} {
							if !strings.Contains(tool.Description, phrase) {
								t.Errorf("internal channel description lacks %q", phrase)
							}
						}
					}
				}
				for name := range workerHiddenTools {
					if names[name] != (profile != "worker") {
						t.Fatalf("%s listing differs for profile %q", name, profile)
					}
				}
				for _, name := range []string{"report_bug", "memory_put", "memory_get", "memory_search"} {
					if !names[name] {
						t.Fatal("worker hid retained tool")
					}
				}
				if profile == "worker" {
					for name := range workerHiddenTools {
						before := up.calls.Load()
						out, err := cli.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
						if err != nil || !out.IsError || out.Content[0].(*mcp.TextContent).Text != "tool-not-in-profile" || up.calls.Load() != before {
							t.Fatalf("%s not rejected locally", name)
						}
					}
				}
			})
		}
	}
}

func TestToolProfileAuthStatusKeepsScopes(t *testing.T) {
	original := authStatus{ScopeDetails: access.ScopeDetails([]string{access.MessageSend})}
	for _, profile := range []string{"full", "worker"} {
		ctx := context.WithValue(t.Context(), toolProfileKey{}, profile)
		out := authVersions(ctx, nil, original)
		if out.ToolProfile != profile || !strings.Contains(out.ToolProfileNote, "hidden from the list only") || !reflect.DeepEqual(out.ScopeDetails, original.ScopeDetails) {
			t.Fatal("profile status altered scopes or lacks label")
		}
	}
}
