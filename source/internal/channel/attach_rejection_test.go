package channel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type rejectingAttachBackend struct {
	*broker.RemoteService
	err error
}

type untrustedRejectionUpstream struct{ result *mcp.CallToolResult }

func (u untrustedRejectionUpstream) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return u.result, nil
}

func TestAttachClientRebuildsGuideFromCodeOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		structured any
		code       string
	}{
		{"known code with hostile guide", map[string]any{"code": "lease-conflict", "message": "synthetic-secret", "detail": "/private/internal"}, "lease-conflict"},
		{"unknown code", map[string]any{"code": "synthetic-secret", "message": "synthetic-secret"}, "attach-rejected"},
		{"old server text only", nil, "attach-rejected"},
		{"wrong field type", map[string]any{"code": 42}, "attach-rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := call(context.Background(), untrustedRejectionUpstream{&mcp.CallToolResult{IsError: true, StructuredContent: tc.structured, Content: []mcp.Content{&mcp.TextContent{Text: "synthetic-secret"}}}}, "attach_session", nil, nil)
			var safe protocol.AttachRejection
			if !errors.As(err, &safe) || safe.Code != tc.code || safe.Message != protocol.SafeAttachRejection(tc.code).Message {
				t.Fatalf("safe rejection missing: %v", err)
			}
			if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "/private/") {
				t.Fatal("client copied remote error content")
			}
		})
	}
}

func TestRemoteAttachClassifiesNameLeaseAndIdentityRejections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	remote := broker.NewRemoteService()
	defer remote.Close()
	base := broker.AttachRequest{Name: "worker", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir(), Harness: "codex", ChannelID: "lease"}
	if _, err := remote.AttachSession(ctx, base); err != nil {
		t.Fatal(err)
	}
	mem, err := memory.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mem.Close(); err != nil {
			t.Error(err)
		}
	})
	client := connectRejectionTest(t, ctx, mcpserver.New(remote, mem))
	nameConflict := base
	nameConflict.SessionID = "22222222-2222-4222-8222-222222222222"
	nameConflict.ChannelID = "another-lease"
	leaseConflict := base
	leaseConflict.PID++
	identityConflict := base
	identityConflict.Name = "changed-name"
	for _, tc := range []struct {
		request broker.AttachRequest
		code    string
	}{
		{nameConflict, "name-in-use"}, {leaseConflict, "lease-conflict"}, {identityConflict, "identity-conflict"}, {broker.AttachRequest{}, "invalid-identity"},
	} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: tc.request})
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(result.StructuredContent)
		var safe protocol.AttachRejection
		if err := json.Unmarshal(data, &safe); err != nil {
			t.Fatal(err)
		}
		if !result.IsError || safe != protocol.SafeAttachRejection(tc.code) {
			t.Fatalf("expected %s, got %+v", tc.code, safe)
		}
	}
}

func (b rejectingAttachBackend) AttachSession(context.Context, broker.AttachRequest) (broker.Session, error) {
	return broker.Session{}, b.err
}

func connectRejectionTest(t *testing.T, ctx context.Context, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "rejection-test", Version: "0.17.3"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestAttachRejectionReasonsCrossBrokerAndClientWithoutInternalErrors(t *testing.T) {
	const private = "synthetic-secret /private/internal.db https://internal.invalid/?token=synthetic-secret"
	for _, tc := range []struct {
		name          string
		err           error
		code, message string
	}{
		{"known name conflict", groups.ErrSelectorConflict, "name-in-use", "Choose another communication name or ask an administrator to detach the old session."},
		{"unknown internal failure", errors.New(private), "attach-rejected", "Registration was rejected. Check the session identity and connection, then try again."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			remote := broker.NewRemoteService()
			defer remote.Close()
			mem, err := memory.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := mem.Close(); err != nil {
					t.Error(err)
				}
			})
			upstream := connectRejectionTest(t, ctx, mcpserver.New(rejectingAttachBackend{remote, tc.err}, mem))
			local := mcp.NewServer(&mcp.Implementation{Name: "local", Version: "1"}, nil)
			addClientToolsWithPolicy(local, upstream, nil, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
				return broker.AttachRequest{SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir(), Harness: "codex", ChannelID: "lease"}, nil
			}, nil, false, nil, nil, nil, nil)
			client := connectRejectionTest(t, ctx, local)
			for _, hop := range []struct {
				name   string
				client *mcp.ClientSession
				args   any
			}{
				{"broker", upstream, broker.AttachRequest{}},
				{"local client", client, map[string]string{"name": "worker"}},
			} {
				result, err := hop.client.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: hop.args})
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(result)
				if strings.Contains(string(encoded), "synthetic-secret") || strings.Contains(string(encoded), "/private/internal.db") {
					t.Errorf("%s leaked an internal error", hop.name)
				}
				data, _ := json.Marshal(result.StructuredContent)
				var safe struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				_ = json.Unmarshal(data, &safe)
				if !result.IsError || safe.Code != tc.code || safe.Message != tc.message {
					t.Errorf("%s: expected safe code/guide %q, got code=%q message=%q isError=%v", hop.name, tc.code, safe.Code, safe.Message, result.IsError)
				}
			}
		})
	}
}
