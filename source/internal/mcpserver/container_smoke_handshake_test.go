package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestContainerSmokeHandshakeProtocol(t *testing.T) {
	b := broker.NewRemoteService()
	defer b.Close()
	server := httptest.NewServer(Handler(newWithCompatibility(b, &stubMemory{}, &serverCompatibility{MinClient: "0.17.3"}, "0.20.0")))
	defer server.Close()
	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "agent-session-broker-client", "version": "0.17.3"}}
	for _, tc := range []struct {
		name, method      string
		headers, metadata bool
		protocol          string
		want              int
	}{
		{"old-smoke-modern-headers", "initialize", true, true, "2026-07-28", 404},
		{"same-json-without-new-headers", "initialize", false, true, "2026-07-28", 400},
		{"legacy-before-895", "initialize", false, false, "2025-03-26", 200},
		{"real-modern-client", "server/discover", true, true, "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{}
			if tc.method == "initialize" {
				params["protocolVersion"] = tc.protocol
				params["capabilities"] = map[string]any{}
				params["clientInfo"] = map[string]any{"name": "container-smoke", "version": "0.17.3"}
			}
			if tc.metadata {
				params["_meta"] = meta
			}
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": tc.method, "params": params})
			req, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/mcp", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tc.headers {
				req.Header.Set("MCP-Protocol-Version", "2026-07-28")
				req.Header.Set("Mcp-Method", tc.method)
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal("real handler request failed")
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal("real handler response failed")
			}
			for _, line := range bytes.Split(data, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data: ")) {
					data = bytes.TrimPrefix(line, []byte("data: "))
					break
				}
			}
			var envelope struct {
				Error *struct {
					Code    int
					Message string
				}
				Result struct {
					ServerInfo any
					Meta       map[string]any `json:"_meta"`
				}
			}
			if json.Unmarshal(data, &envelope) != nil {
				t.Fatal("response must be MCP JSON")
			}
			info := envelope.Result.ServerInfo
			if info == nil {
				info = envelope.Result.Meta["io.modelcontextprotocol/serverInfo"]
			}
			code := 0
			if envelope.Error != nil {
				code = envelope.Error.Code
			}
			t.Logf("HTTP=%d RPC-error=%d serverInfo=%t", resp.StatusCode, code, info != nil)
			if resp.StatusCode != tc.want {
				t.Fatalf("HTTP=%d want=%d", resp.StatusCode, tc.want)
			}
			if tc.name == "real-modern-client" && (envelope.Error != nil || info == nil) {
				t.Fatal("real client discovery failed")
			}
			if tc.name == "old-smoke-modern-headers" && (code != -32601 || envelope.Error.Message != `method not found: "initialize"`) {
				t.Fatalf("wrong refusal source: %s", envelope.Error.Message)
			}
			if tc.name == "same-json-without-new-headers" && code != -32020 {
				t.Fatal("missing protocol header must fail validation")
			}
		})
	}
}

func TestContainerSmokeRequestHelpers(t *testing.T) {
	python, err := exec.LookPath("python3")
	if containerSmokePythonMissing(err) {
		t.Skip("python3 is not available on PATH; Python smoke helpers require it")
	}
	if err != nil {
		t.Fatal(err)
	}
	b := broker.NewRemoteService()
	defer b.Close()
	server := httptest.NewServer(Handler(newWithCompatibility(b, &stubMemory{}, &serverCompatibility{MinClient: "0.17.3"}, "0.20.0")))
	defer server.Close()
	script := `import ast,json,sys,urllib.request,urllib.error
from pathlib import Path
source=Path(sys.argv[2]).read_text();tree=ast.parse(source)
rpc=next(n for n in ast.walk(tree) if isinstance(n,ast.FunctionDef) and n.name=='rpc')
handshake=next(n for n in ast.walk(tree) if isinstance(n,ast.FunctionDef) and n.name in ('initialize','discover'))
params=next(n for n in tree.body if isinstance(n,ast.FunctionDef) and n.name=='request_params')
ns={'json':json,'urllib':urllib,'opener':urllib.request.build_opener(urllib.request.ProxyHandler({})),'admission_token':'','wire_version':'0.17.3'}
exec(compile(ast.Module(body=[params,rpc,handshake],type_ignores=[]),'actual-smoke-helpers','exec'),ns)
try:
 ns[handshake.name](sys.argv[1])
 assert not ns['rpc'](sys.argv[1],'tools/call',{'name':'list_sessions','arguments':{}}).get('isError',False)
 ns['wire_version']='0.0.0'
 assert ns['rpc'](sys.argv[1],'tools/call',{'name':'list_sessions','arguments':{}})['isError']
except urllib.error.HTTPError as e:
 print('actual smoke handshake HTTP',e.code)
 raise SystemExit(1)
`
	input, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "tests", "container_smoke.py"))
	if err != nil {
		t.Fatal("smoke source path")
	}
	requirePrivateScript(t, input)
	output, err := exec.CommandContext(t.Context(), python, "-I", "-c", script, server.URL+"/mcp", input).CombinedOutput()
	if err != nil {
		t.Fatalf("actual Python smoke helpers failed: %s", output)
	}
}

// requirePrivateScript skips only when a private repository script is confirmed
// absent, as in the public source export. Private CI must always run the test.
func requirePrivateScript(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if os.Getenv("GITLAB_CI") != "" {
			t.Fatalf("private CI must provide %s", filepath.Base(path))
		}
		t.Skip("private repository script is not part of the public export")
	}
	if err != nil {
		t.Fatal(err)
	}
}

func containerSmokePythonMissing(err error) bool { return errors.Is(err, exec.ErrNotFound) }

func TestContainerSmokePythonMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := exec.LookPath("python3")
	if !containerSmokePythonMissing(err) {
		t.Fatal("missing python3 must be classified as unavailable")
	}
	for _, err := range []error{nil, os.ErrPermission, exec.ErrDot, errors.New("Python exited unsuccessfully")} {
		if containerSmokePythonMissing(err) {
			t.Fatal("real lookup/execution failure must not be skipped")
		}
	}
}
