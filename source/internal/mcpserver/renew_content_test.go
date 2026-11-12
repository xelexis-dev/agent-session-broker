package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type renewContentBackend struct {
	credentialScopeBackend
	err error
}

func (b *renewContentBackend) RenewLease(context.Context, broker.RenewRequest) (broker.RegisterResult, error) {
	return b.result(), b.err
}

func TestRenewContentSuccessOnly(t *testing.T) {
	for _, code := range []string{"", broker.CodeSuspended, broker.CodeInvalid, broker.CodeNotFound, broker.CodeOwned, broker.CodeStorage, broker.CodeDetached, broker.CodeRevisionMismatch, broker.CodeStaleProbe, broker.CodeCapacity, broker.CodeInvalidToken, "future-business-refusal"} {
		t.Run("code="+code, func(t *testing.T) {
			backend := &renewContentBackend{credentialScopeBackend: credentialScopeBackend{code: code}}
			client := durableClient(t, backend)
			got, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "renew_lease", Arguments: broker.RenewRequest{}})
			if err != nil || got.IsError {
				t.Fatal("renew result")
			}
			want := registrationResult(t.Context(), backend.result(), nil, "dev")
			raw, _ := json.Marshal(got.StructuredContent)

			var decoded broker.RegisterResult
			if json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(decoded, want) {
				t.Fatal("structured content changed")
			}
			if code == "" {
				if len(got.Content) != 0 {
					t.Fatal("successful renew carries duplicate text")
				}
				before := *got
				before.Content = []mcp.Content{&mcp.TextContent{Text: string(raw)}}
				oldBytes, _ := json.Marshal(&before)
				newBytes, _ := json.Marshal(got)
				t.Logf("RENEW_RESULT_BYTES before=%d after=%d saved=%d", len(oldBytes), len(newBytes), len(oldBytes)-len(newBytes))
			} else {
				if len(got.Content) != 1 {
					t.Fatal("business failure text missing")
				}
				text, ok := got.Content[0].(*mcp.TextContent)
				if !ok || text.Text != string(raw) {
					t.Fatal("business failure text changed")
				}
			}
		})
	}
	t.Run("Go-error", func(t *testing.T) {
		client := durableClient(t, &renewContentBackend{err: errors.New("synthetic renewal failure")})
		got, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "renew_lease", Arguments: broker.RenewRequest{}})
		if err != nil || !got.IsError || len(got.Content) != 1 {
			t.Fatal("Go error result changed")
		}
		text, ok := got.Content[0].(*mcp.TextContent)
		if !ok || text.Text != "synthetic renewal failure" {
			t.Fatal("Go error text changed")
		}
	})
}

func TestRenewContentKeepsAuthenticatedScopes(t *testing.T) {
	scopes := []string{access.MessageReceive, access.SessionRegister}
	ctx := access.WithGrant(t.Context(), access.Grant{Scopes: scopes})
	server := New(&renewContentBackend{}, &stubMemory{})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	c, err := mcp.NewClient(&mcp.Implementation{Name: "renew-content", Version: "0.17.3"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "renew_lease", Arguments: broker.RenewRequest{}})
	if err != nil || got.IsError {
		t.Fatal("renew result")
	}
	raw, _ := json.Marshal(got.StructuredContent)
	var v broker.RegisterResult
	if json.Unmarshal(raw, &v) != nil || !reflect.DeepEqual(v.CredentialScopes, scopes) {
		t.Fatal("authenticated scopes lost")
	}
	for _, tc := range []struct {
		name string
		args any
	}{{"register_session", broker.RegisterRequest{}}, {"resume_session", broker.ResumeRequest{}}} {
		r, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err != nil || r.IsError || len(r.Content) != 1 {
			t.Fatal("another tool text changed")
		}
	}
}
