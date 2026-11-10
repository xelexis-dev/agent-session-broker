package mcpserver

// Opt-in reuse of the existing public-client asset cache and named Codex parent
// pattern. No models, real conversations, or non-disposable servers are used.
import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type compatChild struct{ Binary, Endpoint, AuthDir string }

func TestCompatibilityParentHelper(t *testing.T) {
	path := os.Getenv("ASB_COMPAT_CHILD")
	if path == "" {
		t.Skip("named parent helper")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(70)
	}
	var c compatChild
	if json.Unmarshal(data, &c) != nil {
		os.Exit(70)
	}
	cmd := exec.Command(c.Binary, "--broker-url", c.Endpoint, "--token", "synthetic-legacy-credential", "--auth-dir", c.AuthDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, io.Discard
	if cmd.Run() != nil {
		os.Exit(71)
	}
	os.Exit(0)
}
func TestPublicClientCompatibilityMatrix(t *testing.T) {
	assets := os.Getenv("ASB_COMPAT_CLIENT_ASSETS")
	if assets == "" {
		t.Skip("opt-in public release cache")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "codex")
	if os.WriteFile(helper, data, 0700) != nil {
		t.Fatal("helper")
	}
	for _, v := range []string{"0.13.0", "0.14.0", "0.15.0"} {
		name := "asb-client-" + v + "-" + runtime.GOOS + "-" + runtime.GOARCH
		path := filepath.Join(assets, v, name)
		raw, e := os.ReadFile(path)
		if e != nil {
			t.Fatal("cached release client missing")
		}
		metadata, e := os.ReadFile(filepath.Join(assets, v, "release-assets.json"))
		if e != nil {
			t.Fatal(e)
		}
		var release struct {
			Assets []struct{ Name, Digest string }
		}
		if json.Unmarshal(metadata, &release) != nil {
			t.Fatal("release metadata")
		}
		sum := sha256.Sum256(raw)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		verified := false
		for _, a := range release.Assets {
			if a.Name == name && a.Digest == digest {
				verified = true
			}
		}
		if v == "0.15.0" {
			tgz, e := os.ReadFile(filepath.Join(assets, v, "npm.tgz"))
			if e != nil {
				t.Fatal("npm package missing")
			}
			meta, e := os.ReadFile(filepath.Join(assets, v, "registry-dist.json"))
			if e != nil {
				t.Fatal("npm metadata missing")
			}
			var dist struct{ Integrity string }
			if json.Unmarshal(meta, &dist) != nil {
				t.Fatal("npm metadata")
			}
			h := sha512.Sum512(tgz)
			if dist.Integrity != "sha512-"+base64.StdEncoding.EncodeToString(h[:]) {
				t.Fatal("npm integrity mismatch")
			}
			gz, e := gzip.NewReader(bytes.NewReader(tgz))
			if e != nil {
				t.Fatal(e)
			}
			tr := tar.NewReader(gz)
			for {
				h, e := tr.Next()
				if e == io.EOF {
					break
				}
				if e != nil {
					t.Fatal(e)
				}
				if h.Name == "package/bin/"+runtime.GOOS+"-"+runtime.GOARCH+"/asb-client" {
					embedded, e := io.ReadAll(io.LimitReader(tr, 64<<20))
					verified = e == nil && bytes.Equal(embedded, raw)
				}
			}
			gz.Close()
		}
		if !verified {
			t.Fatal("public artifact digest mismatch")
		}
		t.Logf("ASSET %s %s", v, digest)
		for _, restricted := range []bool{true, false} {
			mode := "shipped"
			if restricted {
				mode = "restricted"
			}
			t.Run(v+"/"+mode, func(t *testing.T) {
				t.Parallel()
				backend := broker.NewRemoteService()
				defer backend.Close()
				var policy *serverCompatibility
				if restricted {
					policy = &serverCompatibility{Line: "0.17", MinClient: "0.16.0"}
				}
				server := newWithCompatibility(backend, &stubMemory{}, policy, "0.17.0")
				var callsMu sync.Mutex
				var blockedAt []time.Duration
				began := time.Now()
				if restricted {
					server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
						return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
							if method == "tools/call" {
								callsMu.Lock()
								blockedAt = append(blockedAt, time.Since(began))
								callsMu.Unlock()
							}
							return next(ctx, method, req)
						}
					})
				}

				central := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
				defer central.Close()
				home, cwd, auth := t.TempDir(), t.TempDir(), t.TempDir()
				sid := "11111111-1111-4111-8111-111111111111"
				dir := filepath.Join(home, ".codex", "sessions", "2026", "10", "16")
				os.MkdirAll(dir, 0700)
				meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": sid, "cwd": cwd}})
				os.WriteFile(filepath.Join(dir, "rollout-synthetic-"+sid+".jsonl"), append(meta, '\n'), 0600)
				config, _ := json.Marshal(compatChild{path, central.URL + "/mcp?project=test", auth})
				configPath := filepath.Join(t.TempDir(), "child.json")
				os.WriteFile(configPath, config, 0600)
				cmd := exec.Command(helper, "-test.run=^TestCompatibilityParentHelper$")
				cmd.Dir = cwd
				cmd.Stderr = io.Discard
				cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "CODEX_HOME=" + filepath.Join(home, ".codex"), "CODEX_THREAD_ID=" + sid, "ASB_COMPAT_CHILD=" + configPath}
				ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
				defer cancel()
				cs, e := mcp.NewClient(&mcp.Implementation{Name: "synthetic-host", Version: "0.17.3"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}, nil)
				if e != nil {
					t.Fatal("stdio initialize failed")
				}
				defer cs.Close()
				r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}, Meta: mcp.Meta{"threadId": sid}})
				if e != nil {
					t.Fatal("host tool call failed")
				}
				if !r.IsError {
					t.Fatalf("unexpected host isError=%v", r.IsError)
				}
				if restricted {
					texts := []string{}
					for _, c := range r.Content {
						if x, ok := c.(*mcp.TextContent); ok {
							texts = append(texts, x.Text)
						}
					}
					text := strings.Join(texts, " ")
					if strings.Contains(text, "synthetic-legacy-credential") {
						t.Fatal("credential exposure")
					}
					t.Logf("HOST error: %s", text)
					select {
					case <-ctx.Done():
						t.Fatal("retry observation canceled")
					case <-time.After(120 * time.Second):
					}
					callsMu.Lock()
					offsets := append([]time.Duration(nil), blockedAt...)
					callsMu.Unlock()
					t.Logf("RETRY %s blocked_calls=%d observation=120s offsets=%v", v, len(offsets), offsets)
					if len(offsets) > 60 {
						t.Fatal("restricted old client has a tight retry loop")
					}

				}
				t.Logf("MATRIX %s %s PASS", v, mode)
			})
		}
	}
}
