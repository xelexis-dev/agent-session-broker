package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClientConfigurationFlags(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "asb-client")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/asb-client").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	const token = "synthetic-admission-token_217"
	for _, tc := range []struct {
		name    string
		args    []string
		header  string
		invalid bool
	}{
		{"value", []string{"--token", token}, "Bearer " + token, false},
		{"equals", []string{"--token=" + token}, "Bearer " + token, false},
		{"omitted", nil, "", true},
		{"empty", []string{"--token", ""}, "", true},
		{"spaces", []string{"--token", " " + token}, "", true},
		{"newline", []string{"--token", token + "\r\nInjected: value"}, "", true},
		{"tab", []string{"--token", token + "\t"}, "", true},
		{"too-long", []string{"--token", strings.Repeat("x", 4097)}, "", true},
		{"missing-value", []string{"--token"}, "", true},
		{"legacy-file", []string{"--token-file", filepath.Join(t.TempDir(), "missing-token")}, "", true},
		{"unknown-option", []string{"--" + token}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var headers, bodies []string
			seen := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				headers = append(headers, r.Header.Get("Authorization"))
				bodies = append(bodies, r.URL.String()+string(body))
				mu.Unlock()
				w.WriteHeader(http.StatusUnauthorized)
				select {
				case seen <- struct{}{}:
				default:
				}
			}))
			defer server.Close()
			args := append([]string{"--broker-url", server.URL + "/mcp?project=test", "--state-dir", t.TempDir()}, tc.args...)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out []byte
			if tc.invalid {
				var err error
				out, err = exec.CommandContext(ctx, binary, args...).CombinedOutput()
				if ctx.Err() != nil {
					t.Fatal("invalid configuration did not terminate")
				}
				if err == nil {
					t.Fatal("invalid configuration must fail")
				}
			} else {
				// A broker rejection must not prevent local MCP discovery. Keep
				// stdin open, as the real harness does, until bootstrap has run.
				cmd := exec.CommandContext(ctx, binary, args...)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				client, err := mcp.NewClient(&mcp.Implementation{Name: "flags-fixture", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}, nil)
				if err != nil {
					t.Fatal("local MCP discovery failed")
				}
				listed, err := client.ListTools(ctx, nil)
				if err != nil || len(listed.Tools) == 0 {
					t.Fatal("local catalog unavailable")
				}
				select {
				case <-seen:
				case <-ctx.Done():
					t.Fatal("bootstrap never reached broker")
				}
				_ = client.Close()
				out, _ = json.Marshal(listed)
				out = append(out, stderr.Bytes()...)
			}
			if strings.Contains(string(out), token) {
				t.Fatal("client diagnostics exposed token")
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.invalid {
				if len(headers) != 0 {
					t.Fatal("invalid options reached broker")
				}
				if tc.name == "omitted" && !strings.Contains(string(out), "--token is required") {
					t.Fatal("missing required-token hint")
				}
				if tc.name == "legacy-file" && !strings.Contains(string(out), "--token") {
					t.Fatal("missing migration hint")
				}
				return
			}
			if len(headers) == 0 {
				t.Fatalf("valid options never reached HTTP broker: %s", out)
			}
			for i, header := range headers {
				if header != tc.header {
					t.Fatal("unexpected Authorization header")
				}
				if strings.Contains(bodies[i], token) {
					t.Fatal("token exposed in URL or MCP body")
				}
			}
		})
	}
	help, err := exec.Command(binary, "--help").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"--broker-url", "--token ", "--state-dir"} {
		if !strings.Contains(string(help), option) {
			t.Errorf("help missing %s", option)
		}
	}
	if strings.Contains(string(help), "--token-file") {
		t.Fatal("help still advertises token files")
	}
}
