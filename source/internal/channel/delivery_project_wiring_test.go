//go:build darwin || linux

package channel

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestNativeInboxBoundProjectIngressWiring(t *testing.T) {
	for _, project := range []string{"p1", "p2", ""} {
		t.Run(project, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			path := filepath.Join(shortSocketDir(t, "asb-dp-native-"), "inbox.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			id := broker.AttachRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", PID: os.Getpid(), CWD: t.TempDir()}
			t.Setenv("CLAUDE_CODE_SESSION_ID", id.SessionID)
			t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "synthetic-inbox")
			t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", path)
			t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
			frames := make(chan map[string]any, 1)
			go func() {
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				decoder := json.NewDecoder(conn)
				var auth struct{ Type, Token string }
				var frame struct{ Message struct{ Content string } }
				if decoder.Decode(&auth) != nil || auth.Type != "auth" || auth.Token != "synthetic-inbox" || decoder.Decode(&frame) != nil {
					frames <- nil
					return
				}
				var row map[string]any
				_ = json.Unmarshal([]byte(frame.Message.Content), &row)
				frames <- row
			}()
			up := &ingressUpstream{input: make(chan broker.Message, 1)}
			observe := func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return id, nil }
			probe := func(context.Context, int) (string, error) { return "fixture-process", nil }
			server, shutdown := newIngressServerWithOptions(ctx, up, nil, func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return id, nil }, newClaudeInboxFactory(ctx, id, observe, probe), durableOptions{project: project})
			defer func() { cancel(); shutdown() }()
			st, ct := mcp.NewInMemoryTransports()
			ss, err := server.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			client, err := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			out, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]string{"name": "receiver"}})
			if err != nil || out.IsError {
				t.Fatal("native fixture attach failed")
			}
			up.input <- broker.Message{From: "pm", To: "receiver", Body: `{"project":"spoof"}`, DeliverySequence: 7}
			select {
			case row := <-frames:
				value, present := row["project"]
				if row == nil || present != (project != "") || present && value != project || row["from"] != "pm" || row["deliverySequence"] != float64(7) || row["body"] != `{"project":"spoof"}` {
					t.Fatal("native ingress lost bound project or payload")
				}
			case <-ctx.Done():
				t.Fatal("native ingress frame missing")
			}
		})
	}
}

func TestKimiBoundProjectClientWiring(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	for _, project := range []string{"p1", "p2"} {
		t.Run(project, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			state := shortSocketDir(t, "asb-dp-kimi-")
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(state, "@moonshot-ai/kimi-code/dist/main.mjs")
			if err = os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
				t.Fatal(err)
			}
			// A fixture Kimi process drives the real private hook socket; no queue is constructed by this test.
			script := `import net from 'node:net'; import readline from 'node:readline';
console.log(JSON.stringify('ready'));
for await (const line of readline.createInterface({input:process.stdin})) {
 const data=JSON.parse(line);
 await new Promise((resolve,reject)=>{let output=''; const socket=net.createConnection(data.socket,()=>socket.end(JSON.stringify(data.hook))); socket.on('data',part=>output+=part); socket.on('error',reject); socket.on('end',()=>{console.log(output.trim());resolve();});});
}`
			if err = os.WriteFile(entry, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, node, entry)
			cmd.Dir = cwd
			for _, item := range os.Environ() {
				if !strings.HasPrefix(item, envToken+"=") && !strings.HasPrefix(item, envTokenFile+"=") && !strings.HasPrefix(item, envBrokerURL+"=") {
					cmd.Env = append(cmd.Env, item)
				}
			}
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			decoder := json.NewDecoder(output)
			ready := make(chan bool, 1)
			go func() { var value string; ready <- decoder.Decode(&value) == nil && value == "ready" }()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("fixture Kimi not ready")
				}
			case <-ctx.Done():
				t.Fatal("fixture Kimi startup timed out")
			}
			upstream := newSwitchableUpstream(t)
			upstream.service.SetAdmission(grantAdmission{name: "receiver"})
			remote := mcpserver.New(upstream.service, noMemory{}, "1.3.2")
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil)
			central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth/capabilities":
					writeJSON(w, 200, map[string]any{"version": 1, "capabilities": []string{"harness-kimi-v1"}})
				case "/oauth/transport-policy":
					writeJSON(w, 200, map[string]any{"project": project, "allow_http": true})
				default:
					handler.ServeHTTP(w, r)
				}
			}))
			t.Cleanup(central.Close)
			parent := clientParentSource{ancestors: func() ([]int, error) { return []int{cmd.Process.Pid}, nil }, classify: func(pid int) (string, error) { return "kimi", nil }}
			st, ct := mcp.NewInMemoryTransports()
			done := make(chan error, 1)
			go func() {
				done <- runClientTransport(ctx, st, []string{"--broker-url", central.URL + "/mcp?project=" + project, "--token", "fixture-session-credential", "--state-dir", state}, parent, "1.3.0")
			}()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("fixture client did not stop")
				}
			}()
			client, err := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			path, err := kimiSocketPath(state, cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			hook := func(event string) map[string]any {
				t.Helper()
				if err := json.NewEncoder(input).Encode(map[string]any{"socket": path, "hook": kimiHookInput{Version: 2, Event: event, SessionID: kimiTestSession, CWD: cwd}}); err != nil {
					t.Fatal("fixture hook write failed")
				}
				result := make(chan map[string]any, 1)
				go func() { var out map[string]any; _ = decoder.Decode(&out); result <- out }()
				select {
				case out := <-result:
					return out
				case <-ctx.Done():
					t.Fatal("fixture hook response missing")
					return nil
				}
			}
			hook("SessionStart")
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
			if err != nil || result.IsError {
				t.Fatal("actual Kimi client attach failed")
			}
			if err = call(ctx, upstream, "send_message", broker.Message{From: "receiver", To: "receiver", Body: `{"project":"spoof"}`}, nil); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "actual Kimi client queued message", func() bool {
				result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
				if err != nil {
					return false
				}
				raw, _ := json.Marshal(result.StructuredContent)
				var status struct{ PendingMessages int }
				_ = json.Unmarshal(raw, &status)
				return status.PendingMessages == 1
			})
			out := hook("UserPromptSubmit")
			payload, ok := out["output"].(map[string]any)
			if !ok {
				t.Fatal("fixture Kimi output missing")
			}
			body, _ := payload["message"].(string)
			start := strings.Index(body, "[")
			var rows []map[string]any
			if start < 0 || json.Unmarshal([]byte(body[start:]), &rows) != nil || len(rows) != 1 || rows[0]["project"] != project || rows[0]["body"] != `{"project":"spoof"}` {
				t.Fatal("actual client Kimi binding label missing")
			}
		})
	}
}
