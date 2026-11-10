package mcpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Extracted without edits from 0.17.3:source/internal/channel/tooldefs.json.
func oldSessionSchemas(t *testing.T) map[string]*jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile("testdata/session-output-schemas-0.17.3.json")
	if err != nil {
		t.Fatal(err)
	}
	var schemas map[string]*jsonschema.Schema
	if err := json.Unmarshal(raw, &schemas); err != nil {
		t.Fatal(err)
	}
	resolved := make(map[string]*jsonschema.Resolved)
	for name, schema := range schemas {
		rs, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		resolved[name] = rs
	}
	return resolved
}

func TestActivityOutputMatchesOldClientSchemas(t *testing.T) {
	service := broker.NewRemoteService()
	t.Cleanup(func() { _ = service.Close() })
	register := func(name, id, lease string) broker.RegisterResult {
		out, err := service.RegisterSession(t.Context(), broker.RegisterRequest{Harness: "claude", SessionID: id, PID: 123, CWD: "/work", ClientID: "activity-schema-client", Name: name, ChannelID: lease, Secret: strings.Repeat("s", 40), RequestID: name})
		if err != nil || out.Code != "" {
			t.Fatal("fixture registration failed")
		}
		return out
	}
	caller := register("caller", "11111111-1111-4111-8111-111111111111", "caller-lease")
	target := register("target", "22222222-2222-4222-8222-222222222222", "target-lease")
	sample, _ := json.Marshal(protocol.Activity{State: protocol.ActivityWorking, Source: "codex-rollout", Sequence: 1, ObservedAt: time.Now().UTC()})
	target, err := service.RenewLease(t.Context(), broker.RenewRequest{RegistrationID: target.Session.RegistrationID, ChannelID: "target-lease", Activity: sample})
	if err != nil || target.Code != "" {
		t.Fatal("fixture activity failed")
	}
	backend := service
	// The authenticated session grant, not the requested target/name, selects
	// the caller. Other sessions' samples must not enable this client's output.
	identity, _ := json.Marshal([3]string{"claude", caller.Session.ID, "activity-schema-client"})
	grant := access.Grant{Scopes: access.LegacyV1Scopes(), TokenID: "caller-token", Check: func(id string) error {
		if id != string(identity) {
			return errors.New("not caller")
		}
		return nil
	}}
	grant.Rename = func(id, _, _ string, apply func() error) error {
		if err := grant.Check(id); err != nil {
			return err
		}
		return apply()
	}
	handler := Handler(New(backend, &stubMemory{}))
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(access.WithGrant(r.Context(), grant)))
	}))
	t.Cleanup(central.Close)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "compatibility-test", Version: "0.17.3"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: central.URL + "/mcp", HTTPClient: central.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	schemas := oldSessionSchemas(t)
	cases := []struct {
		name string
		args any
	}{
		{"get_session", map[string]string{"name": "target"}},
		{"rename_session", broker.RenameRequest{RegistrationID: caller.Session.RegistrationID, ChannelID: "caller-lease", Name: "renamed-caller"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil || result.IsError {
				t.Fatal("MCP call failed")
			}
			raw, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var instance any
			if err := json.Unmarshal(raw, &instance); err != nil {
				t.Fatal(err)
			}
			if tc.name == "get_session" {
				schema := schemas[tc.name]
				if schema == nil {
					t.Fatal("missing 0.17.3 schema")
				}
				if err := schema.Validate(instance); err != nil {
					t.Fatalf("0.17.3 output schema rejected structured content: %v", err)
				}
			}
		})
	}
	// Array text has no old output schema; keep the existing activity contract.
	listed := callInto[[]broker.Session](t, client, "list_sessions", map[string]any{})
	for _, s := range listed {
		if s.Name == "target" && (s.Activity == nil || s.Activity.State != protocol.ActivityWorking) {
			t.Fatal("list activity was filtered")
		}
	}
	// A valid unknown/unsupported sample also marks a modern caller.
	unknown, _ := json.Marshal(protocol.Activity{State: protocol.ActivityUnknown, Reason: "unsupported", Sequence: 1, ObservedAt: time.Now().UTC()})
	if out, err := service.RenewLease(t.Context(), broker.RenewRequest{RegistrationID: caller.Session.RegistrationID, ChannelID: "caller-lease", Activity: unknown}); err != nil || out.Code != "" {
		t.Fatal("caller activity failed")
	}
	got := callInto[broker.Session](t, client, "get_session", map[string]string{"name": "target"})
	if got.Activity == nil || got.Activity.State != protocol.ActivityWorking {
		t.Fatal("modern caller lost activity")
	}
	renamed := callInto[broker.Session](t, client, "rename_session", broker.RenameRequest{RegistrationID: caller.Session.RegistrationID, ChannelID: "caller-lease", Name: "modern-caller"})
	if renamed.Activity == nil || renamed.Activity.Reason != "unsupported" {
		t.Fatal("modern rename lost activity")
	}

}
