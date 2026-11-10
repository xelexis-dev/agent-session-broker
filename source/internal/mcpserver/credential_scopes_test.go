package mcpserver

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const scopeCapabilityForTest = "credential-scopes-v1"

type credentialScopeBackend struct {
	scopeBackend
	code string
}

func (b *credentialScopeBackend) result() broker.RegisterResult {
	var out broker.RegisterResult
	// Even a backend-supplied snapshot must not become authentication evidence.
	_ = json.Unmarshal([]byte(`{"credentialScopes":["memory:write"]}`), &out)
	out.Code = b.code
	return out
}
func (b *credentialScopeBackend) RegisterSession(context.Context, broker.RegisterRequest) (broker.RegisterResult, error) {
	return b.result(), nil
}
func (b *credentialScopeBackend) ResumeSession(context.Context, broker.ResumeRequest) (broker.RegisterResult, error) {
	return b.result(), nil
}
func (b *credentialScopeBackend) RenewLease(context.Context, broker.RenewRequest) (broker.RegisterResult, error) {
	return b.result(), nil
}

func TestCredentialScopesSuccessSignalsUseOnlyCurrentGrant(t *testing.T) {
	for _, scopes := range [][]string{{access.MessageReceive, access.SessionRegister}, {access.SessionRegister}, nil} {
		for _, code := range []string{"", broker.CodeOwned} {
			ctx := t.Context()
			if scopes != nil {
				ctx = access.WithGrant(ctx, access.Grant{Scopes: scopes})
			}
			server := New(&credentialScopeBackend{code: code}, &stubMemory{})
			st, ct := mcp.NewInMemoryTransports()
			ss, err := server.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ss.Close() })
			client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil).Connect(t.Context(), ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Close() })
			for _, tc := range []struct {
				name string
				args any
			}{{"register_session", broker.RegisterRequest{}}, {"resume_session", broker.ResumeRequest{}}, {"renew_lease", broker.RenewRequest{}}} {
				out := callInto[map[string]any](t, client, tc.name, tc.args)
				raw, _ := json.Marshal(out)
				var signal struct {
					Scopes []string `json:"credentialScopes"`
				}
				if err := json.Unmarshal(raw, &signal); err != nil {
					t.Fatal(err)
				}
				if code == "" && access.ValidScopes(scopes) {
					if !reflect.DeepEqual(signal.Scopes, scopes) {
						t.Errorf("%s success omitted current authenticated scopes", tc.name)
					}
				} else if _, present := out["credentialScopes"]; present {
					t.Errorf("%s refusal/legacy grant exposed scopes", tc.name)
				}
				var old struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(raw, &old) != nil || old.Code != code {
					t.Error("old client result decode changed")
				}
				if session, ok := out["session"].(map[string]any); ok {
					if _, found := session["credentialScopes"]; found {
						t.Error("scope signal leaked into Session")
					}
				}
			}
		}
	}
}

func TestCredentialScopesCapabilityIsPerGrantAndCopiesSharedCaps(t *testing.T) {
	shared := &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Experimental: map[string]any{"existing": true}}}
	next := projectCapabilityMiddleware(func(context.Context, string, mcp.Request) (mcp.Result, error) { return shared, nil })
	for _, scopes := range [][]string{{access.MessageReceive, access.SessionRegister}, {access.SessionRegister}, nil, {"unknown"}, {access.SessionRegister, access.MessageReceive}} {
		result, err := next(access.WithGrant(t.Context(), access.Grant{Scopes: scopes}), "initialize", nil)
		if err != nil {
			t.Fatal(err)
		}
		caps := result.(*mcp.InitializeResult).Capabilities.Experimental
		v, present := caps[scopeCapabilityForTest]
		if access.ValidScopes(scopes) {
			raw, _ := json.Marshal(v)
			var value struct {
				Scopes []string `json:"scopes"`
			}
			_ = json.Unmarshal(raw, &value)
			if !present || !reflect.DeepEqual(value.Scopes, scopes) {
				t.Error("grant-specific capability omitted or leaked another grant")
			}
		} else if present {
			t.Error("invalid/legacy scopes advertised")
		}
		if _, found := shared.Capabilities.Experimental[scopeCapabilityForTest]; found {
			t.Fatal("shared capability mutated")
		}
	}
	result, _ := next(t.Context(), "initialize", nil)
	if _, found := result.(*mcp.InitializeResult).Capabilities.Experimental[scopeCapabilityForTest]; found {
		t.Fatal("unauthenticated capability disclosed scopes")
	}
	if protocol.CredentialScopesCapability != scopeCapabilityForTest {
		t.Fatal("capability contract changed")
	}
}

func TestCredentialScopesCopyAndDiscovery(t *testing.T) {
	scopes := []string{access.MessageReceive, access.SessionRegister}
	ctx := access.WithGrant(t.Context(), access.Grant{Scopes: scopes, ProjectGenerationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA"})
	shared := &mcp.DiscoverResult{Capabilities: &mcp.ServerCapabilities{Experimental: map[string]any{}}}
	next := projectCapabilityMiddleware(func(context.Context, string, mcp.Request) (mcp.Result, error) { return shared, nil })
	out, err := next(ctx, "server/discover", nil)
	if err != nil {
		t.Fatal(err)
	}
	caps := out.(*mcp.DiscoverResult).Capabilities.Experimental
	signal := caps[protocol.CredentialScopesCapability].(map[string]any)["scopes"].([]string)
	signal[0] = access.MemoryWrite
	if scopes[0] != access.MessageReceive || len(shared.Capabilities.Experimental) != 0 {
		t.Fatal("scope capability shares mutable grant/server state")
	}
	result := credentialScopesResult(ctx, protocol.RegisterResult{CredentialScopes: []string{access.MemoryWrite}}, nil)
	result.CredentialScopes[0] = access.MemoryWrite
	if scopes[0] != access.MessageReceive {
		t.Fatal("result scopes alias grant")
	}
	result = credentialScopesResult(ctx, protocol.RegisterResult{CredentialScopes: scopes}, context.Canceled)
	if result.CredentialScopes != nil {
		t.Fatal("failed result exposed credential scopes")
	}
}

func TestFullGrantWireKeepsReleasedClientUnrestricted(t *testing.T) {
	stored := access.LegacyV1Scopes()
	ctx := access.WithGrant(t.Context(), access.Grant{Scopes: stored})
	got := credentialScopesResult(ctx, protocol.RegisterResult{}, nil).CredentialScopes
	// Released clients classify full grants by this count through 0.19.x.
	if len(got) < 9 || !access.ValidScopes(got) || !access.HasScope(got, access.SessionSpawn) {
		t.Fatal("full grant looks restricted to released clients")
	}
	if access.HasScope(stored, access.SessionSpawn) {
		t.Fatal("wire compatibility mutated stored scopes")
	}
}
