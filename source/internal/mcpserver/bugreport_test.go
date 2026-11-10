package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
)

func TestBugReportRequiresRegisteredCredentialWithoutExtraScope(t *testing.T) {
	for _, mode := range []string{"valid-no-extra-scope", "no-grant", "expired", "revoked", "unbound", "detached"} {
		t.Run(mode, func(t *testing.T) {
			remote := broker.NewRemoteService()
			defer remote.Close()
			req := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/synthetic", ClientID: "fixture-pc", Name: "worker", ChannelID: "fixture-lease", Secret: strings.Repeat("s", 40), RequestID: "fixture-request"}
			result, err := remote.RegisterSession(t.Context(), req)
			if err != nil || result.Code != "" {
				t.Fatal("fixture registration failed")
			}
			if mode == "detached" {
				if err = remote.StopSession(t.Context(), result.Session.Name); err != nil {
					t.Fatal(err)
				}
			}
			store, err := bugreport.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			revoked := make(chan struct{})
			grant := access.Grant{ProjectGenerationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", TokenID: "synthetic", Project: "test", OwnerID: "owner", Scopes: []string{}, Revoked: revoked, ExpiresAt: time.Now().Add(time.Hour), Check: func(identity string) error {
				var fields [3]string
				if json.Unmarshal([]byte(identity), &fields) != nil || fields != [3]string{req.Harness, req.SessionID, req.ClientID} || mode == "unbound" {
					return errors.New("binding refused")
				}
				return nil
			}}
			if mode == "expired" {
				grant.ExpiresAt = time.Now().Add(-time.Hour)
			}
			if mode == "revoked" {
				close(revoked)
			}
			server := New(remote, &stubMemory{}, "0.17.1")
			server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
					ctx = bugreport.WithStore(ctx, store)
					if mode != "no-grant" {
						ctx = access.WithGrant(ctx, grant)
					}
					return next(ctx, method, request)
				}
			})
			client := catalogSession(t, server)
			response, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "report_bug", Arguments: bugreport.Input{Title: "legacy report", Summary: "without client diagnostics"}})
			if err != nil {
				t.Fatal(err)
			}
			reports, total, err := store.List(bugreport.Filter{}, 1)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "valid-no-extra-scope" {
				if !response.IsError || total != 0 {
					t.Fatal("invalid reporter accepted")
				}
				return
			}
			if response.IsError || total != 1 || reports[0].Client != nil || reports[0].Reporter.RegistrationID != result.Session.RegistrationID || reports[0].Reporter.OwnerID != "owner" || reports[0].ServerVersion != "0.17.1" {
				t.Fatal("credential-only legacy report not stored correctly")
			}
			assertBugReportTimeFields(t, response, "receivedAt", "retryAt")
			for i := 0; i < 9; i++ {
				out, e := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "report_bug", Arguments: bugreport.Input{Title: "quota", Summary: "fixture"}})
				if e != nil || out.IsError {
					t.Fatal("quota rejected too early")
				}
			}
			limited, e := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "report_bug", Arguments: bugreport.Input{Title: "quota", Summary: "fixture"}})
			if e != nil || !limited.IsError {
				t.Fatal("rate limit not reported")
			}
			assertBugReportTimeFields(t, limited, "retryAt", "receivedAt")
			raw, _ := json.Marshal(limited.StructuredContent)
			if !strings.Contains(string(raw), "rate-limited") || !strings.Contains(string(raw), "retryAt") {
				t.Fatal("rate limit retry missing")
			}
		})
	}
	if toolScope("report_bug") != credentialOnlyScope || toolScope("unclassified") != "" {
		t.Fatal("credential exception weakened fail-closed policy")
	}
}

// Check the actual SDK wire representations, not only direct struct marshaling.
func assertBugReportTimeFields(t *testing.T, result *mcp.CallToolResult, present, absent string) {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal("structured output marshal failed")
	}
	outputs := map[string][]byte{"structured": raw}
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			outputs["text"] = []byte(text.Text)
		}
	}
	if len(outputs) != 2 {
		t.Fatal("text output missing")
	}
	for kind, raw := range outputs {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("%s output is not JSON", kind)
		}
		if _, exists := fields[absent]; exists {
			t.Errorf("%s output includes zero %s", kind, absent)
		}
		var at time.Time
		if err := json.Unmarshal(fields[present], &at); err != nil || at.IsZero() {
			t.Errorf("%s output missing nonzero %s", kind, present)
		}
	}
}
