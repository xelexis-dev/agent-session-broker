package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
)

// The sentinel is a backend boundary, not a simulated scope decision. Tests
// call the real SDK dispatch with valid arguments and assert entry/no entry.
type scopeBackend struct{ calls atomic.Int64 }

func (b *scopeBackend) RetireSession(context.Context, protocol.RetireRequest) (protocol.RetireResult, error) {
	b.hit()
	return protocol.RetireResult{OK: true}, nil
}
func (b *scopeBackend) hit() { b.calls.Add(1) }
func (b *scopeBackend) ListSessions(context.Context) ([]broker.Session, error) {
	b.hit()
	return []broker.Session{}, nil
}
func (b *scopeBackend) GetSession(context.Context, string) (broker.Session, error) {
	b.hit()
	return broker.Session{Name: "caller", Status: broker.StatusAlive}, nil
}
func (b *scopeBackend) SendMessage(context.Context, broker.Message) (broker.SendResult, error) {
	b.hit()
	return broker.SendResult{OK: true, Accepted: true}, nil
}
func (b *scopeBackend) WaitForMessage(context.Context, string) (broker.Message, error) {
	b.hit()
	return broker.Message{From: "caller", To: "caller", Body: "fixture"}, nil
}
func (b *scopeBackend) StopSession(context.Context, string) error      { b.hit(); return nil }
func (b *scopeBackend) SuspendSession(context.Context, string) error   { b.hit(); return nil }
func (b *scopeBackend) UnsuspendSession(context.Context, string) error { b.hit(); return nil }
func (b *scopeBackend) AttachSession(context.Context, broker.AttachRequest) (broker.Session, error) {
	b.hit()
	return broker.Session{Name: "caller"}, nil
}
func (b *scopeBackend) RegisterSession(context.Context, broker.RegisterRequest) (broker.RegisterResult, error) {
	b.hit()
	return broker.RegisterResult{}, nil
}
func (b *scopeBackend) ResumeSession(context.Context, broker.ResumeRequest) (broker.RegisterResult, error) {
	b.hit()
	return broker.RegisterResult{}, nil
}
func (b *scopeBackend) RenewLease(context.Context, broker.RenewRequest) (broker.RegisterResult, error) {
	b.hit()
	return broker.RegisterResult{}, nil
}
func (b *scopeBackend) AnswerProbe(context.Context, broker.ProbeAnswer) (broker.ProbeResult, error) {
	b.hit()
	return broker.ProbeResult{}, nil
}
func (b *scopeBackend) RenameSession(context.Context, broker.RenameRequest) (broker.Session, error) {
	b.hit()
	return broker.Session{Name: "caller"}, nil
}
func (b *scopeBackend) ReportIngressFailure(context.Context, string, string) error {
	b.hit()
	return nil
}
func (b *scopeBackend) Put(context.Context, memory.PutInput) (memory.Node, error) {
	b.hit()
	return memory.Node{ID: "fixture"}, nil
}
func (b *scopeBackend) Get(context.Context, string) (memory.GetResult, error) {
	b.hit()
	return memory.GetResult{}, nil
}
func (b *scopeBackend) Search(context.Context, memory.SearchInput) (memory.SearchResult, error) {
	b.hit()
	return memory.SearchResult{}, nil
}

func TestCentralToolsScopeAllowDenyTable(t *testing.T) {
	cases := []struct {
		name, scope string
		args        any
	}{
		{"list_sessions", access.SessionRead, struct{}{}},
		{"get_session", access.SessionRead, nameInput{Name: "caller"}},
		{"attach_session", access.SessionRegister, broker.AttachRequest{}},
		{"register_session", access.SessionRegister, broker.RegisterRequest{}},
		{"resume_session", access.SessionRegister, broker.ResumeRequest{}},
		{"renew_lease", access.SessionRegister, broker.RenewRequest{}},
		{"probe_response", access.SessionRegister, broker.ProbeAnswer{}},
		{"report_ingress_failure", access.SessionRegister, map[string]string{"channelId": "lease"}},
		{"rename_session", access.SessionRename, broker.RenameRequest{}},
		{"stop_session", access.SessionManage, nameInput{Name: "caller"}},
		{"retire_session", access.SessionManage, protocol.RetireRequest{Name: "other"}},
		{"suspend_session", access.SessionManage, nameInput{Name: "other"}},
		{"unsuspend_session", access.SessionManage, nameInput{Name: "other"}},
		{"send_message", access.MessageSend, sendInput{From: "caller", To: "caller", Body: "fixture"}},
		{"wait_for_message", access.MessageReceive, waitInput{Recipient: "caller"}},
		{"memory_get", access.MemoryRead, memoryGetInput{ID: "fixture"}},
		{"memory_search", access.MemoryRead, memory.SearchInput{}},
		{"memory_put", access.MemoryWrite, memory.PutInput{Title: "fixture", Body: "fixture", CreatedBy: "caller"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &scopeBackend{}
			server := New(b, b)
			var current atomic.Pointer[access.Grant]
			h := Handler(server)
			central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				if grant := current.Load(); grant != nil {
					ctx = access.WithGrant(ctx, *grant)
				}
				h.ServeHTTP(w, r.WithContext(ctx))
			}))
			defer central.Close()
			client, err := mcp.NewClient(&mcp.Implementation{Name: "old-client-shaped", Version: "0.17.3"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: central.URL + "/mcp", HTTPClient: central.Client()}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			// Send uses two calls (sender lookup then delivery); all other paths one.
			wantCalls := int64(1)
			if tc.name == "send_message" {
				wantCalls = 2
			}
			modes := []string{"required-only", "all-except-required", "legacy-v1", "missing", "local"}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					switch mode {
					case "required-only":
						current.Store(&access.Grant{Scopes: []string{tc.scope}})
					case "all-except-required":
						sc := access.LegacyV1Scopes()
						sc = slices.DeleteFunc(sc, func(s string) bool { return s == tc.scope })
						current.Store(&access.Grant{Scopes: sc})
					case "legacy-v1":
						current.Store(&access.Grant{Scopes: access.LegacyV1Scopes()})
					case "missing":
						current.Store(&access.Grant{})
					case "local":
						current.Store(nil)
					}
					b.calls.Store(0)
					result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
					if err != nil {
						t.Fatal(err)
					}
					denied := mode == "all-except-required" || mode == "missing"
					if denied {
						if !result.IsError || result.StructuredContent != nil || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "insufficient_scope" || b.calls.Load() != 0 {
							t.Fatalf("deny must be fixed, schema-free and side-effect-free; calls=%d", b.calls.Load())
						}
					} else if result.IsError || b.calls.Load() != wantCalls {
						t.Fatalf("permitted operation did not reach backend; calls=%d", b.calls.Load())
					}
				})
			}
			// report_bug and bug report management have their own scope matrix.
			listed, err := client.ListTools(t.Context(), nil)
			if err != nil || len(listed.Tools) != len(cases)+4 {
				t.Fatal("tool inventory changed")
			}
			for _, tool := range listed.Tools {
				if tool.Name == "spawn_session" {
					t.Error("removed spawn must not be listed")
				}
				if toolScope(tool.Name) == "" {
					t.Error("unclassified central tool")
				}
			}
		})
	}
}

func TestUnclassifiedToolDeniedForAuthenticatedCaller(t *testing.T) {
	server := New(stubBackend{}, &stubMemory{})
	var called atomic.Bool
	mcp.AddTool(server, &mcp.Tool{Name: "future_tool"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, okOutput, error) {
		called.Store(true)
		return nil, okOutput{OK: true}, nil
	})
	h := Handler(server)
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(access.WithGrant(r.Context(), access.Grant{Scopes: access.LegacyV1Scopes()})))
	}))
	defer central.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: central.URL + "/mcp", HTTPClient: central.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "future_tool", Arguments: struct{}{}})
	if err != nil || !result.IsError || called.Load() {
		t.Fatal("unclassified tool was not denied")
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), "insufficient_scope") {
		t.Fatal("wrong error")
	}
}

func TestMemoryBackendBoundaryScopes(t *testing.T) {
	b := &scopeBackend{}
	wrapped := scopedMemory{b}
	ctx := access.WithGrant(t.Context(), access.Grant{Scopes: []string{access.SessionRead}})
	if _, e := wrapped.Put(ctx, memory.PutInput{}); e != access.ErrInsufficientScope {
		t.Fatal("write boundary bypass")
	}
	if _, e := wrapped.Get(ctx, "fixture"); e != access.ErrInsufficientScope {
		t.Fatal("read boundary bypass")
	}
	if _, e := wrapped.Search(ctx, memory.SearchInput{}); e != access.ErrInsufficientScope {
		t.Fatal("search boundary bypass")
	}
	if b.calls.Load() != 0 {
		t.Fatal("denial invoked memory backend")
	}
}
