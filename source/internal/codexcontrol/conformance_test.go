//go:build darwin || linux

package codexcontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMain(m *testing.M) {
	if os.Getenv("ASB_CONTROL_TEST_HELPER") == "1" {
		os.Exit(serveControlProbe())
	}
	os.Exit(m.Run())
}
func serveControlProbe() int {
	host, bindErr := Bind(os.Getppid(), processKeyForTest)
	raw, _ := json.Marshal(map[string]int{"pid": os.Getpid(), "parentPID": os.Getppid()})
	if os.WriteFile(os.Getenv("ASB_CONTROL_PARENT_FILE"), raw, 0600) != nil {
		return 1
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "control-conformance", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "verify_control", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if bindErr != nil {
			return nil, ErrUnverifiedHost
		}
		thread, _ := req.Params.Meta["threadId"].(string)
		if thread == "" {
			return nil, fmt.Errorf("probe thread metadata missing")
		}
		c, err := host.Dial(ctx)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		raw, err := c.Call(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 1})
		if err != nil {
			return nil, err
		}
		var turns struct {
			Data []json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &turns) != nil || len(turns.Data) == 0 {
			return nil, fmt.Errorf("probe turns missing")
		}
		out := map[string]any{"verified": true, "turns": len(turns.Data), "mcpPID": os.Getpid(), "parentPID": os.Getppid()}
		text, _ := json.Marshal(out)
		return &mcp.CallToolResult{StructuredContent: out, Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}, nil
	})
	if server.Run(context.Background(), &mcp.StdioTransport{}) != nil {
		return 1
	}
	return 0
}
func TestRealAutoDaemonConformance(t *testing.T) {
	if os.Getenv("ASB_CODEX_CONTROL_E2E") != "1" {
		t.Skip("opt-in isolated installed Codex")
	}
	root, err := os.MkdirTemp(os.TempDir(), "cc-real-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error("owned conformance directory cleanup failed")
		}
	})
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("python3", "testdata/daemon_conformance.py")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if command.Start() != nil {
		t.Fatal("conformance driver start failed")
	}
	decoder := json.NewDecoder(output)
	t.Cleanup(func() {
		_, _ = io.WriteString(input, "stop\n")
		input.Close()
		var cleaned struct {
			OK        bool `json:"ok"`
			Remaining int  `json:"remaining"`
			Models    int  `json:"models"`
		}
		decoded := make(chan error, 1)
		go func() { decoded <- decoder.Decode(&cleaned) }()
		select {
		case err := <-decoded:
			if err != nil || !cleaned.OK || cleaned.Remaining != 0 {
				t.Error("conformance owned-process cleanup failed")
			}
		case <-time.After(20 * time.Second):
			_ = command.Process.Signal(os.Interrupt)
			t.Error("conformance cleanup timed out")
		}
		if command.Wait() != nil {
			t.Error("conformance driver failed")
		}
	})
	if json.NewEncoder(input).Encode(map[string]string{"root": root, "binary": binary}) != nil {
		t.Fatal("driver input failed")
	}
	var ready struct {
		OK        bool   `json:"ok"`
		Stage     string `json:"stage"`
		ParentPID int    `json:"parentPID"`
		MCPPID    int    `json:"mcpPID"`
		ThreadID  string `json:"threadId"`
	}
	if decoder.Decode(&ready) != nil || !ready.OK {
		t.Fatalf("isolated daemon setup failed: %s", ready.Stage)
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_HOME", "")
	host, err := Bind(ready.ParentPID, processKeyForTest)
	if err != nil {
		t.Fatal(err)
	}
	c, err := host.Dial(t.Context())
	if err != nil {
		t.Fatal("outer verified controller failed", err)
	}
	defer c.Close()
	raw, err := c.CallMCPTool(t.Context(), map[string]any{"threadId": ready.ThreadID, "server": "control-probe", "tool": "verify_control", "arguments": map[string]any{}})
	if err != nil {
		t.Fatal("real MCP probe call failed", err)
	}
	var answer struct {
		IsError    bool `json:"isError"`
		Structured struct {
			Verified  bool `json:"verified"`
			Turns     int  `json:"turns"`
			MCPPID    int  `json:"mcpPID"`
			ParentPID int  `json:"parentPID"`
		} `json:"structuredContent"`
	}
	if json.Unmarshal(raw, &answer) != nil || answer.IsError || !answer.Structured.Verified || answer.Structured.Turns < 1 || answer.Structured.MCPPID != ready.MCPPID || answer.Structured.ParentPID != ready.ParentPID {
		t.Fatal("real MCP parent verifier or turns/list failed")
	}
	t.Logf("auto-daemon verified: peer/MCP-parent PID=%d MCP PID=%d turns=%d", ready.ParentPID, ready.MCPPID, answer.Structured.Turns)
}
