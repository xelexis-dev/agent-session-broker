package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestScopeInfoPreservesRemoteCatalog(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	got, e := catalogSession(t, New(remote, &stubMemory{})).ListTools(t.Context(), nil)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(got)
	if e != nil {
		t.Fatal(e)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	if path := os.Getenv("ASB_SCOPE_CATALOG_DUMP"); path != "" {
		if os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("dump failed")
		}
	}
	if hash != "5aaac0e80d5f9d1140d27ba7b4c41e58d43a92dd4200aa835f7c45b6e77a09fb" {
		t.Fatal("public remote catalog changed")
	}
	t.Logf("catalogSHA256=%s", hash)
}

func TestTokenInfoIsHiddenAndRequiresOnlyAValidGrant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grant  *access.Grant
		denied bool
	}{
		{"restricted", &access.Grant{TokenID: "synthetic", Scopes: []string{access.SessionRegister}, AdmissionMode: "manual", ExpiresAt: time.Now().Add(time.Hour)}, false},
		{"no-grant", nil, true},
		{"expired", &access.Grant{TokenID: "synthetic", Scopes: []string{access.SessionRegister}, ExpiresAt: time.Now().Add(-time.Hour)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := broker.NewRemoteService()
			defer remote.Close()
			server := New(remote, &stubMemory{})
			if tc.grant != nil {
				server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
						return next(access.WithGrant(ctx, *tc.grant), method, req)
					}
				})
			}
			client := catalogSession(t, server)
			listed, e := client.ListTools(t.Context(), nil)
			if e != nil {
				t.Fatal(e)
			}
			for _, tool := range listed.Tools {
				if tool.Name == access.TokenInfoTool {
					t.Fatal("credential diagnostic advertised")
				}
			}
			result, e := client.CallTool(t.Context(), &mcp.CallToolParams{Name: access.TokenInfoTool, Arguments: struct{}{}})
			if e != nil || result.IsError != tc.denied {
				t.Fatal("credential diagnostic authorization")
			}
			if !tc.denied {
				raw, _ := json.Marshal(result.StructuredContent)
				var info access.TokenInfo
				if json.Unmarshal(raw, &info) != nil || !reflect.DeepEqual(info.Scopes, tc.grant.Scopes) || info.AdmissionMode != "manual" || !info.ExpiresAt.Equal(tc.grant.ExpiresAt) {
					t.Fatal("scope snapshot changed")
				}
			}
		})
	}
}
