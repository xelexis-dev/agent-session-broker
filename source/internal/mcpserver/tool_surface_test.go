package mcpserver

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type surfaceBackend struct {
	*broker.RemoteService
	attachments bool
}

func TestServerToolSurfaceUpdateRejectedInCI(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("test executable unavailable")
	}
	for _, key := range []string{"CI", "GITLAB_CI"} {
		for _, value := range []string{"true", "false", ""} {
			t.Run(key+"/"+value, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, "testdata"), 0755); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "testdata", "server-tool-surface.json")
				original := []byte("{\"synthetic\":\"must-not-change\"}\n")
				if err := os.WriteFile(path, original, 0644); err != nil {
					t.Fatal(err)
				}
				command := exec.CommandContext(t.Context(), executable, "-test.run=^TestServerToolSurfaceGolden$")
				command.Dir = dir
				for _, entry := range os.Environ() {
					if !strings.HasPrefix(entry, "CI=") && !strings.HasPrefix(entry, "GITLAB_CI=") && !strings.HasPrefix(entry, "ASB_UPDATE_SERVER_SURFACE=") {
						command.Env = append(command.Env, entry)
					}
				}
				command.Env = append(command.Env, "ASB_UPDATE_SERVER_SURFACE=1", key+"="+value)
				output, err := command.CombinedOutput()
				if err == nil || !bytes.Contains(output, []byte("golden update is disabled in CI")) {
					t.Error("CI update must fail with the explicit refusal")
				}
				current, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(current, original) {
					t.Error("CI update changed the mismatched fixture")
				}
			})
		}
	}
}

func (b surfaceBackend) AttachmentsAvailable() bool { return b.attachments }

type serverToolSurface struct {
	Name         string `json:"name"`
	InputSchema  any    `json:"inputSchema"`
	OutputSchema any    `json:"outputSchema"`
}

func TestServerToolSurfaceGolden(t *testing.T) {
	remote := broker.NewRemoteService()
	defer remote.Close()
	var tools []serverToolSurface
	caps := map[string]*mcp.ServerCapabilities{}
	for _, profile := range []string{"default", "attachments"} {
		client := catalogSession(t, New(surfaceBackend{remote, profile == "attachments"}, &stubMemory{}))
		listed, err := client.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal("list tools failed")
		}
		sort.Slice(listed.Tools, func(i, j int) bool { return listed.Tools[i].Name < listed.Tools[j].Name })
		current := make([]serverToolSurface, 0, len(listed.Tools))
		for _, tool := range listed.Tools {
			current = append(current, serverToolSurface{tool.Name, tool.InputSchema, tool.OutputSchema})
		}
		if tools == nil {
			tools = current
		} else {
			a, _ := json.Marshal(tools)
			b, _ := json.Marshal(current)
			if !bytes.Equal(a, b) {
				t.Fatal("attachment flag unexpectedly changes advertised tool schemas")
			}
		}
		caps[profile] = client.InitializeResult().Capabilities
	}
	data, err := json.MarshalIndent(struct {
		Tools        []serverToolSurface                `json:"tools"`
		Capabilities map[string]*mcp.ServerCapabilities `json:"capabilities"`
	}{tools, caps}, "", "  ")
	if err != nil {
		t.Fatal("surface encoding failed")
	}
	data = append(data, '\n')
	const path = "testdata/server-tool-surface.json"
	// Explicit review-only update: never rewrite a baseline during a normal run.
	if os.Getenv("ASB_UPDATE_SERVER_SURFACE") == "1" {
		for _, key := range []string{"CI", "GITLAB_CI"} {
			if _, set := os.LookupEnv(key); set {
				t.Fatal("golden update is disabled in CI")
			}
		}
		if os.WriteFile(path, data, 0644) != nil {
			t.Fatal("golden update failed")
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("golden read failed")
	}
	if !bytes.Equal(data, golden) {
		t.Fatal("server tool names, schemas or capabilities changed; review and explicitly update ASB_UPDATE_SERVER_SURFACE=1")
	}
}
