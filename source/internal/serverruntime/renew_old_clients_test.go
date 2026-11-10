package serverruntime

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Reuse the isolated opt-in matrix with clients built from the exact tag trees.
func TestOldClientsStructuredOnlyRenew(t *testing.T) {
	assets := os.Getenv("ASB_OLD_CLIENT_ASSETS")
	if assets == "" {
		t.Skip("opt-in: tag-built client assets required")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("named Codex helper requires Unix")
	}
	self, err := os.Executable()
	matrixRequire(t, err == nil, "helper executable")
	helper := filepath.Join(t.TempDir(), "codex")
	data, err := os.ReadFile(self)
	matrixRequire(t, err == nil && os.WriteFile(helper, data, 0700) == nil, "helper copy")
	for _, version := range []string{"0.12.0", "0.17.3"} {
		t.Run(version, func(t *testing.T) {
			binary := matrixBinary(t, assets, version)
			dir, home, auth, cwd := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			rollout := filepath.Join(home, ".codex", "sessions", "2026", "10", "14")
			matrixRequire(t, os.MkdirAll(rollout, 0700) == nil, "rollout directory")
			meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": matrixSID, "cwd": cwd}})
			matrixRequire(t, os.WriteFile(filepath.Join(rollout, "rollout-synthetic-"+matrixSID+".jsonl"), append(meta, '\n'), 0600) == nil, "synthetic identity")
			store, err := clientstate.Open(auth)
			matrixRequire(t, err == nil, "local store")
			cid, err := store.ClientID()
			matrixRequire(t, err == nil, "client identity")
			_, token, _, _, _ := matrixSeed(t, dir, cid)
			handler, closeRuntime, err := newManagedHandler(dir, nil)
			if err != nil {
				msg := regexp.MustCompile(`asb_[psu]_[A-Za-z0-9_-]+`).ReplaceAllString(err.Error(), "[redacted]")
				msg = regexp.MustCompile(`/[^\s:]+`).ReplaceAllString(msg, "[path]")
				t.Fatalf("runtime start: %s", msg)
			}
			defer closeRuntime()
			expectText := os.Getenv("ASB_RENEW_EXPECT_TEXT") == "1"
			var renews, resumes atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/mcp" {
					handler.ServeHTTP(w, r)
					return
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "synthetic read", 400)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var request struct {
					Method string
					Params struct{ Name string }
				}
				_ = json.Unmarshal(raw, &request)
				if request.Method != "tools/call" {
					handler.ServeHTTP(w, r)
					return
				}
				if request.Params.Name == "resume_session" {
					resumes.Add(1)
				}
				if request.Params.Name != "renew_lease" {
					handler.ServeHTTP(w, r)
					return
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				var envelope struct{ Result *mcp.CallToolResult }
				payload := rec.Body.Bytes()
				// Both JSON and SSE are valid streamable HTTP response encodings.
				if json.Unmarshal(payload, &envelope) != nil {
					for _, line := range bytes.Split(payload, []byte("\n")) {
						if bytes.HasPrefix(line, []byte("data: ")) {
							_ = json.Unmarshal(bytes.TrimPrefix(line, []byte("data: ")), &envelope)
						}
					}
				}
				if envelope.Result != nil && !envelope.Result.IsError && ((!expectText && len(envelope.Result.Content) == 0) || (expectText && len(envelope.Result.Content) == 1)) && envelope.Result.StructuredContent != nil {
					raw, _ := json.Marshal(envelope.Result.StructuredContent)
					var renewed protocol.RegisterResult
					if json.Unmarshal(raw, &renewed) == nil && renewed.Code == "" && renewed.Session.RegistrationID != "" {
						if renews.Add(1) == 1 {
							before := *envelope.Result
							before.Content = []mcp.Content{&mcp.TextContent{Text: string(raw)}}
							oldBytes, _ := json.Marshal(&before)
							after := *envelope.Result
							after.Content = []mcp.Content{}
							newBytes, _ := json.Marshal(&after)
							actual, _ := json.Marshal(envelope.Result)
							t.Logf("RENEW_WIRE %s text=%t actual=%d before=%d after=%d saved=%d", version, expectText, len(actual), len(oldBytes), len(newBytes), len(oldBytes)-len(newBytes))
						}
					}
				}
				for k, values := range rec.Header() {
					for _, v := range values {
						w.Header().Add(k, v)
					}
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write(rec.Body.Bytes())
			}))
			defer server.Close()
			endpoint := server.URL + "/mcp?project=test"
			scope := strings.ToLower(server.URL) + "/mcp?project=test"
			key := clientstate.SessionKey(scope, "codex", matrixSID, cid)
			matrixRequire(t, store.SaveSession(key, clientstate.SessionState{Scope: scope, Project: "test", Harness: "codex", SessionID: matrixSID, ClientID: cid, Registration: clientstate.Registration{Secret: clientstate.NewSecret()}, Credential: &clientstate.SessionCredential{AccessToken: token, TokenType: "Bearer", Nickname: "session", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}) == nil, "saved session fixture")
			c := matrixStart(t, binary, helper, endpoint, token, auth, home, cwd)
			defer c.close()
			if version == "0.17.3" {
				matrixRequire(t, c.tool(t.Context(), "attach_session", map[string]any{}, nil), "explicit attach")
			}
			// A second successful heartbeat without resume proves the first was decoded.
			matrixWait(t, "two structured-only renewals", func() bool { return renews.Load() >= 2 })
			matrixRequire(t, resumes.Load() == 0, "no decode-failure recovery")
			var own protocol.Session
			matrixRequire(t, c.tool(t.Context(), "get_session", map[string]string{"name": "session"}, &own) && own.AutomaticDeliveryReady, "old client remains ready")

			if version == "0.17.3" {
				listed, e := c.session.ListTools(t.Context(), nil)
				matrixRequire(t, e == nil, "old client catalog")
				for _, tool := range listed.Tools {
					if tool.Name == "send_message" {
						raw, _ := json.Marshal(tool.InputSchema)
						var schema struct{ Properties map[string]json.RawMessage }
						matrixRequire(t, json.Unmarshal(raw, &schema) == nil && schema.Properties["attachment"] == nil, "old client send schema unchanged")
					}
				}
				matrixRequire(t, c.tool(t.Context(), "ingress_hook", map[string]string{"session_id": matrixSID, "hook_event_name": "PreToolUse"}, nil), "old client hook start")
				var sent protocol.SendResult
				matrixRequire(t, c.tool(t.Context(), "send_message", protocol.Message{From: "session", To: "session", Body: "synthetic-attachment-compat-text"}, &sent) && sent.Accepted, "old client plain send")
				matrixWait(t, "old client plain receive", func() bool {
					var hook map[string]any
					if !c.tool(t.Context(), "ingress_hook", map[string]string{"session_id": matrixSID, "hook_event_name": "PostToolUse"}, &hook) {
						return false
					}
					raw, _ := json.Marshal(hook)
					return strings.Contains(string(raw), "synthetic-attachment-compat-text")
				})
			}
			t.Logf("MATRIX %s text=%t renew PASS successes=%d resumes=%d", version, expectText, renews.Load(), resumes.Load())
		})
	}
}
