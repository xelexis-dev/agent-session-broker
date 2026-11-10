//go:build darwin

package serverruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/processinfo"
)

// Own real stdio processes only; synthetic Claude identity, inbox and credentials.
func TestClientExitWhileReceiving(t *testing.T) {
	if os.Getenv("ASB_CLIENT_EXIT_BINARY") == "" {
		t.Skip("opt-in real-binary lifecycle test: ASB_CLIENT_EXIT_BINARY")
	}
	binary := os.Getenv("ASB_CLIENT_EXIT_BINARY")
	dir := t.TempDir()
	helper := filepath.Join(dir, "claude")
	source := filepath.Join(dir, "main.go")
	code := strings.Replace(strings.Replace(upgradeClaudeSource, "cmd.Stdin,cmd.Stdout,cmd.Stderr=os.Stdin,os.Stdout,io.Discard", "cmd.Stdin,cmd.Stdout,cmd.Stderr=os.Stdin,os.Stdout,os.Stderr;_ = io.Discard", 1), "if cmd.Run()!=nil{os.Exit(71)}", `check(cmd.Start()==nil);check(os.WriteFile(filepath.Join(home,"client-pid"),[]byte(strconv.Itoa(cmd.Process.Pid)),0600)==nil);if cmd.Wait()!=nil{os.Exit(71)}`, 1)
	matrixRequire(t, os.WriteFile(source, []byte(code), 0600) == nil, "parent source")
	matrixRequire(t, exec.Command("go", "build", "-o", helper, source).Run() == nil, "parent build")
	for _, launcher := range []bool{true} {
		for _, end := range []string{"eof", "sigterm"} {
			label := end
			if launcher {
				label = "execve-" + label
			}
			if !t.Run(label, func(t *testing.T) {
				home, auth, cwd, data := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
				store, e := clientstate.Open(auth)
				matrixRequire(t, e == nil, "state open")
				clientID, e := store.ClientID()
				matrixRequire(t, e == nil, "client id")
				control, e := controlpanel.OpenSQLite(filepath.Join(data, "control"))
				matrixRequire(t, e == nil, "control open")
				matrixRequire(t, control.Setup("synthetic-exit-password") == nil && control.CreateProject("test") == nil, "control setup")
				matrixRequire(t, control.SetInstanceHTTPPolicy("admin", true) == nil, "HTTP fixture")
				parent, e := control.IssueProjectTokenFor("admin", "test", "synthetic")
				matrixRequire(t, e == nil, "parent token")
				code, e := control.BeginDeviceAuthorization("test", parent, controlpanel.DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: matrixSID, InstanceID: clientID, RequestID: "exit-fixture"})
				matrixRequire(t, e == nil, "device seed")
				_, e = control.ApproveDeviceAuthorization("admin", code.UserCode, "exit-fixture", false)
				matrixRequire(t, e == nil, "approval seed")
				grant, e := control.ExchangeDeviceToken("test", parent, code.DeviceCode, "asb-client-v1", false)
				matrixRequire(t, e == nil, "grant seed")
				matrixRequire(t, control.Close() == nil, "seed close")
				handler, closeRuntime, e := newManagedHandler(data, nil)
				matrixRequire(t, e == nil, "runtime")
				waiting := make(chan struct{}, 1)
				heldTool := make(chan struct{}, 1)
				httpStopped := make(chan struct{}, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/mcp") {
						raw, _ := io.ReadAll(r.Body)
						r.Body = io.NopCloser(bytes.NewReader(raw))
						if bytes.Contains(raw, []byte(`"get_session"`)) {
							select {
							case heldTool <- struct{}{}:
							default:
							}
							<-r.Context().Done()
							httpStopped <- struct{}{}
							return
						}
						if bytes.Contains(raw, []byte(`"wait_for_message"`)) {
							select {
							case waiting <- struct{}{}:
							default:
							}
						}
					}
					handler.ServeHTTP(w, r)
				}))
				defer server.Close()
				defer closeRuntime()
				endpoint := strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "/mcp?project=test"
				key := clientstate.SessionKey(strings.ToLower(endpoint), "claude", matrixSID, clientID)
				matrixRequire(t, store.SaveSession(key, clientstate.SessionState{Scope: strings.ToLower(endpoint), Project: "test", Harness: "claude", SessionID: matrixSID, ClientID: clientID, Registration: clientstate.Registration{Secret: clientstate.NewSecret()}, Credential: &clientstate.SessionCredential{AccessToken: grant.AccessToken, TokenType: "Bearer", Nickname: "exit-fixture", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}) == nil, "stored grant")
				target := binary
				if launcher {
					node, e := exec.LookPath("node")
					matrixRequire(t, e == nil, "node available")
					out, e := exec.Command(node, "-p", "typeof process.execve").Output()
					if e != nil || strings.TrimSpace(string(out)) != "function" {
						t.Skip("Node execve unavailable")
					}
					// Existing package launcher and the real binary, without npm registry/network.
					pkg := filepath.Join(t.TempDir(), "package")
					slot := "darwin-arm64"
					if runtime.GOARCH == "amd64" {
						slot = "darwin-x64"
					}
					matrixRequire(t, os.MkdirAll(filepath.Join(pkg, "bin", slot), 0700) == nil, "launcher slot")
					b, e := os.ReadFile(binary)
					matrixRequire(t, e == nil && os.WriteFile(filepath.Join(pkg, "bin", slot, "asb-client"), b, 0700) == nil, "launcher binary")
					b, e = os.ReadFile("../../npm/x-broker.js")
					matrixRequire(t, e == nil && os.WriteFile(filepath.Join(pkg, "bin", "x-broker.js"), b, 0700) == nil, "launcher copy")
					matrixRequire(t, os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"synthetic-exit","version":"0.17.0","bin":{"x-broker":"bin/x-broker.js"}}`), 0600) == nil, "launcher metadata")
					target = filepath.Join(pkg, "run")
					matrixRequire(t, os.WriteFile(target, []byte("#!/bin/sh\nexec '"+node+"' '"+filepath.Join(pkg, "bin", "x-broker.js")+"' \"$@\"\n"), 0700) == nil, "launcher shim")
				}

				npm, e := exec.LookPath("npm")
				matrixRequire(t, e == nil, "npm available")
				// Build a local-only package; never publish or change global npm.
				// target is <package>/run.
				packageDir := filepath.Dir(target)
				packDir := t.TempDir()
				pack := exec.Command(npm, "pack", "--ignore-scripts", "--pack-destination", packDir)
				pack.Dir = packageDir
				matrixRequire(t, pack.Run() == nil, "local npm pack")
				tarballs, _ := filepath.Glob(filepath.Join(packDir, "*.tgz"))
				matrixRequire(t, len(tarballs) == 1, "local tarball")
				npmShim := filepath.Join(home, "npm-shim")
				cache := filepath.Join(home, "npm-cache")
				script := "#!/bin/sh\nexport NPM_CONFIG_USERCONFIG=/dev/null\nexec '" + npm + "' exec --offline --ignore-scripts --yes --cache '" + cache + "' --package '" + tarballs[0] + "' -- x-broker \"$@\"\n"
				matrixRequire(t, os.WriteFile(npmShim, []byte(script), 0700) == nil, "npm shim")
				target = npmShim

				// Node hosts create socket-backed stdio, unlike os/exec's anonymous pipes.
				node, e := exec.LookPath("node")
				matrixRequire(t, e == nil, "Node host")
				bridge := filepath.Join(home, "bridge.cjs")
				bridgeSource := `const {spawn}=require('node:child_process');const fs=require('node:fs');const path=require('node:path');const child=spawn(process.argv[2],process.argv.slice(3),{stdio:['pipe','pipe','pipe']});fs.writeFileSync(path.join(process.env.HOME,'actual-pid'),String(child.pid));process.stdin.pipe(child.stdin);child.stdout.pipe(process.stdout);child.stderr.pipe(process.stderr);child.on('exit',()=>process.exit(0));`
				matrixRequire(t, os.WriteFile(bridge, []byte(bridgeSource), 0600) == nil, "Node host source")
				shim := filepath.Join(home, "bridge")
				matrixRequire(t, os.WriteFile(shim, []byte("#!/bin/sh\nexec '"+node+"' '"+bridge+"' '"+target+"' \"$@\"\n"), 0700) == nil, "Node host shim")
				target = shim
				inbox := newUpgradeInbox(t)
				config, _ := json.Marshal(matrixChildConfig{target, endpoint, parent, auth})
				path := filepath.Join(home, "child.json")
				matrixRequire(t, os.WriteFile(path, config, 0600) == nil, "child config")
				cmd := exec.Command(helper, "--synthetic-parent", path)
				cmd.Dir = cwd
				cmd.Env = []string{"HOME=" + home, "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "CLAUDE_CODE_SESSION_ID=" + matrixSID, "CLAUDE_CODE_MESSAGING_TOKEN=" + inbox.token, "CLAUDE_CODE_MESSAGING_SOCKET=" + inbox.path}
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				stdin, e := cmd.StdinPipe()
				matrixRequire(t, e == nil, "stdin")
				stdout, e := cmd.StdoutPipe()
				matrixRequire(t, e == nil, "stdout")
				matrixRequire(t, cmd.Start() == nil, "parent start")
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				var child *os.Process
				defer func() {
					if child != nil {
						_ = child.Kill()
					}
					_ = cmd.Process.Kill()
					_ = stdin.Close()
				}()
				// Cold npm setup is separate from the 3s shutdown deadline.
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				local, e := mcp.NewClient(&mcp.Implementation{Name: "exit-fixture", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: stdout, Writer: stdin}, nil)
				matrixRequire(t, e == nil, "initialize")
				defer local.Close()
				// A seeded credential needs the user's explicit first registration.
				matrixRequire(t, (&matrixClient{local}).tool(ctx, "attach_session", map[string]any{}, nil), "explicit initial registration")
				// Reconnect uses a different parent here; see update-20261025-hostchain-optin-test.md.
				select {
				case <-waiting:
				case <-ctx.Done():
					t.Fatal("receiver did not enter HTTP wait")
				}
				pidBytes, e := os.ReadFile(filepath.Join(home, "actual-pid"))
				matrixRequire(t, e == nil, "child pid")
				pid, e := strconv.Atoi(string(pidBytes))
				matrixRequire(t, e == nil, "pid parse")
				pid = exitClientDescendant(t, pid)
				child, e = os.FindProcess(pid)
				matrixRequire(t, e == nil, "owned child")
				unlock, e := store.LockSession(key)
				if e == nil {
					unlock()
					t.Fatal("live binding did not hold conn.lock")
				}
				// A host call may still be outstanding when reconnect closes stdin.
				callStarted := make(chan struct{})
				go func() {
					close(callStarted)
					_, _ = local.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_session", Arguments: map[string]any{"name": "exit-fixture"}})
				}()
				<-callStarted
				select {
				case <-heldTool:
				case <-time.After(3 * time.Second):
					t.Fatal("forwarded host wait missing")
				}
				start := time.Now()
				if end == "eof" {
					_ = stdin.Close()
				} else {
					matrixRequire(t, child.Signal(syscall.SIGTERM) == nil, "owned SIGTERM")
				}
				select {
				case <-done:
					unlock, e = store.LockSession(key)
					matrixRequire(t, e == nil, "conn.lock released")
					unlock()
					select {
					case <-httpStopped:
					case <-time.After(time.Second):
						t.Fatal("upstream HTTP request remained after client exit")
					}
					t.Logf("exit=%s elapsed_ms=%d lockReleased=true httpCanceled=true", label, time.Since(start).Milliseconds())
				case <-time.After(3 * time.Second):
					_ = child.Signal(syscall.SIGQUIT)
					time.Sleep(100 * time.Millisecond)
					// Function names only, never stack arguments, env, body or raw errors.
					for _, line := range strings.Split(stderr.String(), "\n") {
						if strings.Contains(line, "(") && (strings.HasPrefix(line, "github.com/") || strings.HasPrefix(line, "net/http.") || strings.HasPrefix(line, "sync.")) {
							t.Log(strings.Split(line, "(")[0])
						}
					}
					t.Fatal("client exceeded 3s exit deadline during receive")
				}
			}) {
				return
			}
		}
	}
}

// Restrict inspection to the owned fixture tree; never read or print argv/env.
func exitClientDescendant(t *testing.T, root int) int {
	t.Helper()
	queue := []int{root}
	for depth := 0; len(queue) > 0 && depth < 32; depth++ {
		pid := queue[0]
		queue = queue[1:]
		info, e := processinfo.Read(pid)
		if e == nil && filepath.Base(info.Executable) == "asb-client" {
			return pid
		}
		out, _ := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
		for _, text := range strings.Fields(string(out)) {
			if n, e := strconv.Atoi(text); e == nil {
				queue = append(queue, n)
			}
		}
	}
	t.Fatal("owned asb-client descendant missing")
	return 0
}
