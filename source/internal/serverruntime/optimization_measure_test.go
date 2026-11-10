//go:build darwin

package serverruntime

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

// Opt-in only; profiles and all runtime credentials stay in a disposable root.
func TestOptimizationMeasure(t *testing.T) {
	mode := os.Getenv("ASB_OPT_MEASURE")
	if mode == "" {
		t.Skip("opt-in isolated optimization measurement")
	}
	root, e := os.MkdirTemp("/private/tmp/optm", "r")
	if e != nil {
		t.Fatal("temp")
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	source, _ := filepath.Abs("../..")
	overlay := optOverlay(t, source, root)
	build := func(name, pkg string) string {
		out := filepath.Join(root, name)
		c := exec.Command("go", "build", "-overlay", overlay, "-o", out, pkg)
		c.Dir = source
		c.Env = append(os.Environ(), "GOROOT="+filepath.Join(root, "goroot"))
		if output, err := c.CombinedOutput(); err != nil {
			t.Fatalf("overlay build %s: %s", name, output)
		}
		return out
	}
	client := build("asb-client", "./cmd/asb-client")
	serverBinary := build("asb-server", "./cmd/asb-server")
	parentSrc := filepath.Join(root, "parent.go")
	os.WriteFile(parentSrc, []byte(optParent), 0600)
	os.MkdirAll(filepath.Join(root, "bin"), 0700)
	parent := filepath.Join(root, "bin", "codex")
	if exec.Command("go", "build", "-o", parent, parentSrc).Run() != nil {
		t.Fatal("parent build")
	}
	claude := filepath.Join(root, "bin", "claude")
	raw, _ := os.ReadFile(parent)
	os.WriteFile(claude, raw, 0700)
	launcher := ""
	if mode == "M3" {
		d := filepath.Join(root, "npm")
		os.MkdirAll(filepath.Join(d, "bin", "darwin-arm64"), 0700)
		raw, _ := os.ReadFile(filepath.Join(source, "npm/x-broker.js"))
		launcher = filepath.Join(d, "bin/x-broker.js")
		os.WriteFile(launcher, raw, 0600)
		os.WriteFile(filepath.Join(d, "package.json"), []byte(`{"name":"synthetic-opt-launcher","version":"0.17.3"}`), 0600)
		raw, _ = os.ReadFile(client)
		os.WriteFile(filepath.Join(d, "bin/darwin-arm64/asb-client"), raw, 0700)
	}
	duration := 10 * time.Minute
	if mode == "N2" {
		duration = 5 * time.Second
	}
	if mode == "M3" {
		duration = 10 * time.Second
	}
	if os.Getenv("ASB_OPT_SMOKE") == "1" {
		duration = 5 * time.Second
	}
	run := func(t *testing.T, label string, n int, harness string, size int) {
		t.Helper()
		dir := filepath.Join(root, label)
		os.MkdirAll(dir, 0700)
		seed, e := controlpanel.OpenSQLite(filepath.Join(dir, "data/control"))
		if e != nil {
			t.Fatal("seed")
		}
		if seed.Setup("synthetic-measurement-password") != nil || seed.CreateProject("probe") != nil || seed.SetInstanceHTTPPolicy("admin", true) != nil {
			t.Fatal("setup")
		}
		tokens := make([]string, n)
		for i := range tokens {
			tokens[i], e = seed.IssueTokenFor("admin", "probe", fmt.Sprint(i), "admin")
			if e != nil {
				t.Fatal("credential")
			}
		}
		seed.Close()
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal("listen")
		}
		addr := l.Addr().String()
		l.Close()
		ss := filepath.Join(root, label+"s.sock")
		srv := exec.Command(serverBinary, "--listen", addr, "--memory-dir", filepath.Join(dir, "data"))
		srv.Env = optEnv(dir, ss)
		srv.Stdout = io.Discard
		srv.Stderr = io.Discard
		if srv.Start() != nil {
			t.Fatal("server start")
		}
		defer func() { srv.Process.Signal(os.Interrupt); srv.Wait() }()
		until := time.Now().Add(15 * time.Second)
		for {
			r, e := http.Get("http://" + addr + "/healthz")
			if e == nil {
				r.Body.Close()
				break
			}
			if time.Now().After(until) {
				t.Fatal("health")
			}
			time.Sleep(50 * time.Millisecond)
		}
		type bound struct {
			s     *mcp.ClientSession
			sock  string
			start map[string]any
			pid   int
		}
		clients := []bound{}
		defer func() {
			for _, b := range clients {
				b.s.Close()
			}
		}()
		for i := 0; i < n; i++ {
			home := filepath.Join(dir, fmt.Sprint(i))
			os.MkdirAll(home, 0700)
			state := filepath.Join(home, "state")
			sock := filepath.Join(root, fmt.Sprintf("%sc%d.sock", label, i))
			sid := fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)
			if harness == "codex" {
				roll := filepath.Join(home, ".codex/sessions/2026/10/30")
				os.MkdirAll(roll, 0700)
				meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": sid, "cwd": home}})
				f, _ := os.Create(filepath.Join(roll, "rollout-probe-"+sid+".jsonl"))
				f.Write(append(meta, '\n'))
				line := []byte(`{"type":"event_msg","payload":{"type":"agent_message","message":"` + strings.Repeat("x", 900) + `"}}` + "\n")
				for written := len(meta) + 1; written < size; written += len(line) {
					f.Write(line)
				}
				if strings.HasPrefix(label, "valid") {
					for _, kind := range []string{"task_started", "task_complete"} {
						v, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "type": "event_msg", "payload": map[string]string{"type": kind, "turn_id": "synthetic-idle"}})
						f.Write(append(v, '\n'))
					}
				}
				f.Close()
				if strings.Contains(label, "broad") {
					for k := 0; k < 414; k++ {
						d := filepath.Join(home, ".codex/sessions/2026", fmt.Sprintf("%02d", 1+(k%37)/28), fmt.Sprintf("%02d", 1+(k%37)%28))
						os.MkdirAll(d, 0700)
						name := fmt.Sprintf("rollout-fake-22222222-2222-4222-8222-%012d.jsonl", k)
						os.WriteFile(filepath.Join(d, name), []byte("{}\n"), 0600)
					}
				}
			}
			helper := parent
			if harness == "claude" {
				helper = claude
			}
			cmd := exec.Command(helper)
			if harness == "kimi" {
				js := filepath.Join(root, "kimi.cjs")
				os.WriteFile(js, []byte(`process.title="kimi-code"; const c=require('node:child_process').spawn(process.env.OPT_BINARY,["--broker-url",process.env.OPT_ENDPOINT,"--token",process.env.OPT_TOKEN,"--state-dir",process.env.OPT_STATE],{stdio:'inherit'});c.on('exit',n=>process.exit(n||0));`), 0600)
				cmd = exec.Command("node", js)
			}
			cmd.Dir = home
			cmd.Env = append(optEnv(home, sock), "OPT_BINARY="+client, "OPT_ENDPOINT=http://"+addr+"/mcp?project=probe", "OPT_TOKEN="+tokens[i], "OPT_STATE="+state, "CODEX_THREAD_ID="+sid, "CLAUDE_CODE_SESSION_ID="+sid, "CLAUDE_CODE_MESSAGING_SOCKET="+filepath.Join(home, "inbox.sock"), "CLAUDE_CODE_MESSAGING_TOKEN=synthetic-inbox")
			if label == "claude-channel" {
				var env []string
				for _, v := range cmd.Env {
					if !strings.HasPrefix(v, "CLAUDE_CODE_MESSAGING_") {
						env = append(env, v)
					}
				}
				cmd.Env = env
			}
			if launcher != "" {
				cmd.Env = append(cmd.Env, "OPT_LAUNCHER="+launcher)
			}
			if label == "npx" {
				d := filepath.Join(home, "node_modules/.bin")
				os.MkdirAll(d, 0700)
				os.WriteFile(filepath.Join(home, "package.json"), []byte(`{"name":"synthetic-opt","version":"0.0.0","private":true}`), 0600)
				os.WriteFile(filepath.Join(d, "x-broker"), []byte(optNodeBootstrap), 0700)
				os.WriteFile(filepath.Join(home, ".npmrc"), nil, 0600)
				os.WriteFile(filepath.Join(home, "global.npmrc"), nil, 0600)
				cmd.Env = append(cmd.Env, "OPT_NPX=1", "NPM_CONFIG_USERCONFIG="+filepath.Join(home, ".npmrc"), "NPM_CONFIG_GLOBALCONFIG="+filepath.Join(home, "global.npmrc"), "NPM_CONFIG_CACHE="+filepath.Join(home, ".npm"), "NPM_CONFIG_LOGS_MAX=0", "NPM_CONFIG_LOGLEVEL=silent")
			}

			cmd.Stderr = io.Discard
			started := time.Now()
			s, e := mcp.NewClient(&mcp.Implementation{Name: "synthetic-parent", Version: "1"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}, nil)
			if e != nil {
				t.Fatal("initialize", label)
			}
			if i == 0 {
				fmt.Printf("OPT_STARTUP %s %d\n", label, time.Since(started).Milliseconds())
			}
			if mode == "N2" && i == 0 {
				fmt.Printf("OPT2_INSTRUCTIONS %s bytes=%d estimatedTokens=%.2f\n", label, len(s.InitializeResult().Instructions), float64(len(s.InitializeResult().Instructions))/4)
				catalog, e := s.ListTools(t.Context(), nil)
				if e != nil {
					t.Fatal("catalog")
				}
				opt2Size(label, "tools/list", catalog)
				for _, tool := range catalog.Tools {
					input, _ := json.Marshal(tool.InputSchema)
					output, _ := json.Marshal(tool.OutputSchema)
					all, _ := json.Marshal(tool)
					fmt.Printf("OPT2_TOOL %s %s name=%d description=%d inputSchema=%d outputSchema=%d total=%d\n", label, tool.Name, len(tool.Name), len(tool.Description), len(input), len(output), len(all))
				}
				if harness == "kimi" {
					out, e := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
					if e != nil {
						t.Fatal("kimi status")
					}
					opt2Size(label, "auth_status-unregistered", out)
					s.Close()
					return
				}
			}
			r, e := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}, Meta: mcp.Meta{"threadId": sid}})
			if e != nil || r.IsError {
				t.Fatal("attach", label)
			}
			time.Sleep(100 * time.Millisecond)
			v := optSample(t, sock, "s")
			clients = append(clients, bound{s: s, sock: sock, pid: int(v["pid"].(float64))})
		}
		if mode == "N2" {
			cli := clients[0].s
			for _, name := range []string{"auth_status", "list_sessions"} {
				out, e := cli.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
				if e != nil || out.IsError {
					t.Fatal("output measurement", name)
				}
				opt2Size(label, name, out)
				if name == "list_sessions" {
					var sessions []struct{ Name string }
					if len(out.Content) == 0 || json.Unmarshal([]byte(out.Content[0].(*mcp.TextContent).Text), &sessions) != nil || len(sessions) != 5 {
						t.Fatal("expected five sessions")
					}
					one, e := cli.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_session", Arguments: map[string]any{"name": sessions[0].Name}})
					if e != nil || one.IsError {
						t.Fatal("get_session")
					}
					opt2Size(label, "get_session", one)
				}
			}
		}
		if mode == "N1" {
			time.Sleep(20 * time.Second)
		} else {
			time.Sleep(time.Second)
		}
		for i := range clients {
			clients[i].start = optSample(t, clients[i].sock, "s")
		}
		if mode == "M3" {
			for _, b := range clients {
				out, e := exec.Command("ps", "-o", "pid=,ppid=,rss=", "-p", strconv.Itoa(b.pid)).Output()
				if e != nil {
					t.Fatal("launcher process")
				}
				fmt.Printf("OPT_LAUNCHER_CLIENT %s", out)
				launchRaw, e := os.ReadFile(filepath.Join(dir, "0", "launch.pid"))
				if e != nil {
					t.Fatal("launcher pid")
				}
				launchPID, _ := strconv.Atoi(strings.TrimSpace(string(launchRaw)))
				fmt.Printf("OPT_LAUNCHER_ID initialPID=%d clientPID=%d samePID=%t\n", launchPID, b.pid, launchPID == b.pid)
				if launchPID != b.pid {
					out, e := exec.Command("ps", "-o", "pid=,ppid=,rss=", "-p", strconv.Itoa(launchPID)).Output()
					if e == nil {
						fmt.Printf("OPT_NODE_RESIDENT %s", out)
					}
				}
				var pid, ppid, rss int
				fmt.Sscan(string(out), &pid, &ppid, &rss)
				out, e = exec.Command("pgrep", "-P", strconv.Itoa(ppid)).Output()
				if e != nil {
					t.Fatal("launcher siblings")
				}
				for _, v := range strings.Fields(string(out)) {
					out, e := exec.Command("ps", "-o", "pid=,ppid=,rss=", "-p", v).Output()
					if e == nil {
						fmt.Printf("OPT_LAUNCHER_CHILD %s", out)
					}
				}
			}
		}
		serverStart := optSample(t, ss, "s")
		if mode == "N3" {
			opt2DBFiles(t, filepath.Join(dir, "data"), "start")
		}
		start := time.Now()
		fmt.Printf("OPT_BEGIN %s clients=%d seconds=%.0f\n", label, n, duration.Seconds())
		profileSock := ss
		if mode == "M1" || mode == "M1GRID" || mode == "N1" {
			profileSock = clients[0].sock
		}
		profDone := make(chan struct{})
		go func() {
			defer close(profDone)
			if duration < time.Minute {
				return
			}
			c, e := net.Dial("unix", profileSock)
			if e != nil {
				return
			}
			defer c.Close()
			c.Write([]byte("p"))
			f, _ := os.Create(filepath.Join(dir, "cpu.pprof"))
			io.Copy(f, c)
			f.Close()
		}()
		timer := time.NewTimer(duration)
		<-timer.C
		<-profDone
		elapsed := time.Since(start).Seconds()
		if mode == "M3" {
			raw, e := os.ReadFile(filepath.Join(dir, "0", "launch.pid"))
			if e != nil {
				t.Fatal("end launcher pid")
			}
			id := strings.TrimSpace(string(raw))
			out, e := exec.Command("ps", "-o", "pid=,ppid=,rss=", "-p", id).Output()
			if e != nil {
				t.Fatal("launcher vanished during live session")
			}
			fmt.Printf("OPT_LAUNCHER_END %s %s", label, out)
		}
		for _, b := range clients {
			optReport(t, label, "client", b.start, optSample(t, b.sock, "s"), elapsed)
		}
		optReport(t, label, "server", serverStart, optSample(t, ss, "s"), elapsed)
		if mode == "N3" {
			opt2DBFiles(t, filepath.Join(dir, "data"), "end")
			before := optSample(t, ss, "s")
			raw, _ := json.Marshal(before)
			fmt.Printf("OPT2_MEMORY before-panel %s\n", raw)
			c, e := net.Dial("unix", ss)
			if e != nil {
				t.Fatal("heap socket")
			}
			c.Write([]byte("m"))
			heap := filepath.Join(dir, "heap.pprof")
			f, e := os.Create(heap)
			if e != nil {
				t.Fatal("heap file")
			}
			_, e = io.Copy(f, c)
			f.Close()
			c.Close()
			if e != nil {
				t.Fatal("heap read")
			}
			out, e := exec.Command("go", "tool", "pprof", "-top", "-lines", "-inuse_space", "-nodecount=15", serverBinary, heap).Output()
			if e != nil {
				t.Fatal("heap top")
			}
			fmt.Printf("OPT2_HEAP\n%s\n", out)
			jar, _ := cookiejar.New(nil)
			browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			login, e := browser.Get("http://" + addr + "/login")
			if e != nil {
				t.Fatal("login page")
			}
			io.Copy(io.Discard, login.Body)
			login.Body.Close()
			csrf := ""
			for _, cookie := range login.Cookies() {
				if cookie.Name == "asb_form" {
					csrf = cookie.Value
				}
			}
			if csrf == "" {
				t.Fatal("login form cookie")
			}
			login, e = browser.PostForm("http://"+addr+"/login", url.Values{"username": {"admin"}, "password": {"synthetic-measurement-password"}, "csrf": {csrf}})
			if e != nil {
				t.Fatal("login")
			}
			io.Copy(io.Discard, login.Body)
			login.Body.Close()
			if login.StatusCode != 303 {
				t.Fatal("admin login status")
			}
			r, e := browser.Get("http://" + addr + "/admin/settings")
			if e != nil {
				t.Fatal("panel GET")
			}
			pageBytes, _ := io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode != 200 || pageBytes < 1000 {
				t.Fatal("admin page status/body")
			}
			fmt.Printf("OPT2_PANEL authenticated-admin-settings bytes=%d\n", pageBytes)
			after := optSample(t, ss, "s")
			raw, _ = json.Marshal(after)
			fmt.Printf("OPT2_MEMORY after-panel status=%d %s\n", r.StatusCode, raw)
			for _, phase := range []string{"after-panel"} {
				out, e := exec.Command("ps", "-o", "pid=,rss=", "-p", strconv.Itoa(srv.Process.Pid)).Output()
				if e != nil {
					t.Fatal("panel RSS")
				}
				fmt.Printf("OPT2_RSS %s %s", phase, out)
			}
		}
		if duration >= time.Minute {
			binary := serverBinary
			if mode == "M1" || mode == "M1GRID" || mode == "N1" {
				binary = client
			}
			c := exec.Command("go", "tool", "pprof", "-top", "-lines", "-nodecount=18", binary, filepath.Join(dir, "cpu.pprof"))
			out, e := c.Output()
			if e != nil {
				t.Fatal("pprof")
			}
			fmt.Printf("OPT_PPROF %s\n%s\n", label, out)
		}
	}
	if mode == "N2" {
		run(t, "codex", 5, "codex", 5<<20)
		run(t, "claude-native", 5, "claude", 0)
		run(t, "claude-channel", 5, "claude", 0)
		run(t, "kimi", 1, "kimi", 0)
	} else if mode == "N1" {
		t.Run("valid-broad", func(t *testing.T) { t.Parallel(); run(t, "valid-broad", 1, "codex", 50<<20) })
		t.Run("claude", func(t *testing.T) { t.Parallel(); run(t, "claude", 1, "claude", 0) })
	} else if mode == "N3" {
		run(t, "server30", 30, "codex", 5<<20)
	} else if mode == "M1GRID" {
		for _, label := range []string{"valid-small", "valid-broad", "missing-small", "missing-broad"} {
			t.Run(label, func(t *testing.T) { t.Parallel(); run(t, label, 1, "codex", 50<<20) })
		}
	} else if mode == "M1" {
		for _, v := range []struct {
			label, harness string
			size           int
		}{{"codex5", "codex", 5 << 20}, {"codex50", "codex", 50 << 20}, {"claude", "claude", 0}} {
			t.Run(v.label, func(t *testing.T) { t.Parallel(); run(t, v.label, 1, v.harness, v.size) })
		}
	} else if mode == "M3" {
		run(t, "launcher", 1, "codex", 5<<20)
		run(t, "npx", 1, "codex", 5<<20)
	} else if mode == "M2" {
		for _, n := range []int{1, 10, 30} {
			run(t, fmt.Sprintf("server%d", n), n, "codex", 5<<20)
		}
	} else {
		t.Fatal("mode")
	}
}

func optEnv(home, sock string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"), "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"), "TMPDIR=/private/tmp/optm", "OPT_SOCKET=" + sock}
}
func optSample(t *testing.T, sock, command string) map[string]any {
	t.Helper()
	c, e := net.Dial("unix", sock)
	if e != nil {
		t.Fatal("sample dial")
	}
	defer c.Close()
	c.Write([]byte(command))
	var v map[string]any
	if json.NewDecoder(c).Decode(&v) != nil {
		t.Fatal("sample decode")
	}
	return v
}
func optReport(t *testing.T, label, kind string, a, b map[string]any, seconds float64) {
	t.Helper()
	delta := map[string]any{"label": label, "kind": kind, "seconds": seconds, "pid": b["pid"]}
	for k, v := range b {
		if k == "pid" {
			continue
		}
		if n, ok := v.(float64); ok {
			prior, _ := a[k].(float64)
			delta[k] = n - prior
		}
	}
	pid := int(b["pid"].(float64))
	out, e := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if e != nil {
		t.Fatal("rss")
	}
	rss, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	delta["rssKiB"] = rss
	raw, _ := json.Marshal(delta)
	fmt.Printf("OPT_RESULT %s\n", raw)
}

func optOverlay(t *testing.T, source, root string) string {
	t.Helper()
	replace := map[string]string{}
	put := func(path, content string) {
		p := filepath.Join(root, fmt.Sprintf("o%d.go", len(replace)))
		if os.WriteFile(p, []byte(content), 0600) != nil {
			t.Fatal("overlay write")
		}
		replace[path] = p
	}
	goroot := filepath.Join(root, "goroot")
	os.MkdirAll(goroot, 0700)
	if exec.Command("cp", "-R", filepath.Join(runtime.GOROOT(), "src"), filepath.Join(goroot, "src")).Run() != nil {
		t.Fatal("isolated toolchain source")
	}
	for _, name := range []string{"bin", "pkg", "lib"} {
		if os.Symlink(filepath.Join(runtime.GOROOT(), name), filepath.Join(goroot, name)) != nil {
			t.Fatal("toolchain link")
		}
	}
	osdir := filepath.Join(goroot, "src/os")
	raw, e := os.ReadFile(filepath.Join(osdir, "file.go"))
	if e != nil {
		t.Fatal("os source")
	}
	text := string(raw)
	for _, method := range []string{"Read", "ReadAt", "Write", "WriteAt"} {
		needle := "func (f *File) " + method + "("
		a := strings.Index(text, needle)
		if a < 0 {
			t.Fatal("os signature")
		}
		b := strings.Index(text[a:], "{\n") + a + 2
		kind := "read"
		if strings.HasPrefix(method, "Write") {
			kind = "write"
		}
		text = text[:b] + " defer func(){if n>0 {OptProbeIO(f.Name(),\"" + kind + "\",uint64(n))}}()\n" + text[b:]
	}
	put(filepath.Join(osdir, "file.go"), text)
	put(filepath.Join(osdir, "opt_probe.go"), optOS)
	files, _ := filepath.Glob(filepath.Join(source, "internal/channel/*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(f)
		s := string(raw)
		changed := false
		for _, name := range []string{"ticker", "tick"} {
			needle := "case <-" + name + ".C:"
			if strings.Contains(s, needle) {
				s = strings.ReplaceAll(s, needle, needle+" optOS.OptProbeEvent(\"tick:"+filepath.Base(f)+"\");")
				changed = true
			}
		}
		if changed {
			s = strings.Replace(s, "import (", "import (\noptOS \"os\"", 1)
			put(f, s)
		}
	}
	for _, name := range []string{"asb-client", "asb-server"} {
		p := filepath.Join(source, "cmd", name, "main.go")
		raw, _ := os.ReadFile(p)
		s := strings.Replace(string(raw), "import (", "import (\noptJSON \"encoding/json\"\noptNet \"net\"\noptPprof \"runtime/pprof\"\noptTime \"time\"\noptSys \"syscall\"", 1) + optMain
		put(p, s)
	}
	// The wrapper counts route/tool names, never headers, query values or bodies.
	p := filepath.Join(source, "internal/serverruntime/run.go")
	raw, _ = os.ReadFile(p)
	s := strings.Replace(string(raw), "import (", "import (\noptOS \"os\"\noptJSON \"encoding/json\"\noptBytes \"bytes\"", 1)
	needle := "server := &http.Server{"
	if !strings.Contains(s, needle) {
		t.Fatal("server signature")
	}
	s = strings.Replace(s, needle, "handler = optMeasureHandler(handler)\n"+needle, 1) + optHTTP
	put(p, s)
	p = filepath.Join(source, "internal/localactivity/observer.go")
	raw, _ = os.ReadFile(p)
	s = strings.Replace(string(raw), "import (", "import (\noptOS \"os\"", 1)
	s = strings.Replace(s, "after, probeErr :=", `optOS.OptProbeAdd("observeBytes",uint64(limit-b.left));optOS.OptProbeEvent("observations");if o.sequence==1 {optOS.OptProbeAdd("firstObservationBytes",uint64(limit-b.left))}
 after, probeErr :=`, 1)
	put(p, s)
	p = filepath.Join(source, "internal/sqlitestore/snapshot.go")
	raw, _ = os.ReadFile(p)
	s = strings.Replace(string(raw), "return pfs.Rename(temporary, path)", `if err=pfs.Rename(temporary,path);err!=nil{return err};os.OptProbeEvent("dbRewrites:"+filepath.Base(path));os.OptProbeAdd("dbWriteBytes:"+filepath.Base(path),uint64(len(data)));return nil`, 1)
	s = strings.Replace(s, "return change(s.db)", `err:=change(s.db);if err==nil {os.OptProbeEvent("dbDirectMutations:"+filepath.Base(s.filePath))};return err`, 1)
	if s == string(raw) {
		t.Fatal("snapshot instrumentation signature")
	}
	put(p, s)
	p = filepath.Join(source, "internal/broker/remote_durable.go")
	raw, _ = os.ReadFile(p)
	s = strings.Replace(string(raw), "import (", "import (\noptOS \"os\"", 1)
	if strings.Contains(s, "if s.persistLocked(e.record) == nil {") {
		s = strings.Replace(s, "if s.persistLocked(e.record) == nil {", "if s.persistLocked(e.record) == nil {optOS.OptProbeEvent(\"lastSeenPersist\");", 1)
	} else {
		s = strings.Replace(s, "e.dirty = true", "e.dirty = true;optOS.OptProbeEvent(\"lastSeenDirty\");", 1)
	}
	put(p, s)
	out, _ := json.Marshal(map[string]any{"Replace": replace})
	path := filepath.Join(root, "overlay.json")
	os.WriteFile(path, out, 0600)
	return path
}

const optOS = `package os
import("sync";"strings";"runtime")
var optProbeMu sync.Mutex
var optProbeCounts=map[string]uint64{}
func OptProbeAdd(k string,n uint64){optProbeMu.Lock();optProbeCounts[k]+=n;optProbeMu.Unlock()}
func OptProbeEvent(k string){optProbeMu.Lock();optProbeCounts[k]++;optProbeMu.Unlock()}
func OptProbeIO(p,k string,n uint64){if !strings.HasPrefix(p,Getenv("HOME")+"/"){return};optProbeMu.Lock();defer optProbeMu.Unlock();if k=="read"{optProbeCounts["fileReadBytes"]+=n;if strings.Contains(p,"rollout-"){optProbeCounts["rolloutReadBytes"]+=n}}else if state:=Getenv("OPT_STATE");state!=""&&strings.HasPrefix(p,state+"/"){optProbeCounts["stateWriteCalls"]++;optProbeCounts["stateWriteBytes"]+=n}}
func OptProbeSnapshot()map[string]uint64{optProbeMu.Lock();defer optProbeMu.Unlock();v:=map[string]uint64{};for k,n:=range optProbeCounts{v[k]=n};var m runtime.MemStats;runtime.ReadMemStats(&m);v["HeapAlloc"]=m.HeapAlloc;v["HeapInuse"]=m.HeapInuse;v["Sys"]=m.Sys;v["StackInuse"]=m.StackInuse;v["StackSys"]=m.StackSys;v["GCSys"]=m.GCSys;return v}
`
const optMain = `
func init(){p:=os.Getenv("OPT_SOCKET");if p==""{return};l,e:=optNet.Listen("unix",p);if e!=nil{panic("probe socket")};os.Chmod(p,0600);go func(){for{c,e:=l.Accept();if e!=nil{return};go func(){defer c.Close();var b[1]byte;c.Read(b[:]);if b[0]=='m'{optPprof.WriteHeapProfile(c);return};if b[0]=='p'{if optPprof.StartCPUProfile(c)!=nil{return};optTime.Sleep(60*optTime.Second);optPprof.StopCPUProfile();return};v:=map[string]any{};for k,n:=range os.OptProbeSnapshot(){v[k]=n};var r optSys.Rusage;optSys.Getrusage(optSys.RUSAGE_SELF,&r);v["cpuSeconds"]=float64(r.Utime.Sec+r.Stime.Sec)+float64(r.Utime.Usec+r.Stime.Usec)/1e6;v["pid"]=os.Getpid();optJSON.NewEncoder(c).Encode(v)}()}}()}
`
const optHTTP = `
func optMeasureHandler(next http.Handler)http.Handler{return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){name:=r.URL.Path;if name=="/mcp"&&r.Body!=nil {raw,_:=io.ReadAll(io.LimitReader(r.Body,1<<20));r.Body.Close();r.Body=io.NopCloser(optBytes.NewReader(raw));var v struct{Method string;Params struct{Name string}};if optJSON.Unmarshal(raw,&v)==nil {if v.Params.Name!="" {name+="/"+v.Params.Name}else{name+="/"+v.Method}}};optOS.OptProbeEvent("route:"+name);next.ServeHTTP(w,r)})}
`
const optParent = `package main
import("os";"os/exec";"encoding/json";"path/filepath";"strconv")
func main(){home:=os.Getenv("HOME");if filepath.Base(os.Args[0])=="claude" {d:=filepath.Join(home,".claude/sessions");os.MkdirAll(d,0700);cwd,_:=os.Getwd();raw,_:=json.Marshal(map[string]any{"sessionId":os.Getenv("CLAUDE_CODE_SESSION_ID"),"kind":"interactive","pid":os.Getpid(),"cwd":cwd});os.WriteFile(filepath.Join(d,strconv.Itoa(os.Getpid())+".json"),raw,0600)};args:=[]string{"--broker-url",os.Getenv("OPT_ENDPOINT"),"--token",os.Getenv("OPT_TOKEN"),"--state-dir",os.Getenv("OPT_STATE")};binary:=os.Getenv("OPT_BINARY");if launcher:=os.Getenv("OPT_LAUNCHER");launcher!="" {binary="node";args=append([]string{launcher},args...)};if os.Getenv("OPT_NPX")=="1" {binary="npx";args=[]string{"--no-install","--offline","--","x-broker"}};c:=exec.Command(binary,args...);c.Stdin=os.Stdin;c.Stdout=os.Stdout;c.Stderr=os.Stderr;if c.Start()!=nil{os.Exit(2)};os.WriteFile(filepath.Join(home,"launch.pid"),[]byte(strconv.Itoa(c.Process.Pid)),0600);if c.Wait()!=nil{os.Exit(2)}}
`

const optNodeBootstrap = `#!/usr/bin/env node
process.argv=[process.execPath,process.env.OPT_LAUNCHER,"--broker-url",process.env.OPT_ENDPOINT,"--token",process.env.OPT_TOKEN,"--state-dir",process.env.OPT_STATE];require(process.env.OPT_LAUNCHER).main();
`

func opt2Size(harness, label string, value any) {
	raw, e := json.Marshal(value)
	if e != nil {
		panic("measure marshal")
	}
	fmt.Printf("OPT2_BYTES %s %s bytes=%d estimatedTokens=%.2f\n", harness, label, len(raw), float64(len(raw))/4)
	if result, ok := value.(*mcp.CallToolResult); ok {
		textBytes := 0
		for _, part := range result.Content {
			if text, ok := part.(*mcp.TextContent); ok {
				textBytes += len(text.Text)
			}
		}
		structured, _ := json.Marshal(result.StructuredContent)
		structuredBytes := len(structured)
		if result.StructuredContent == nil {
			structuredBytes = 0
		}
		fmt.Printf("OPT2_RESULT_PARTS %s %s textBytes=%d structuredBytes=%d\n", harness, label, textBytes, structuredBytes)
	}
}

func opt2DBFiles(t *testing.T, root, phase string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".db") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"path": relative, "bytes": info.Size(), "mtimeUnixNs": info.ModTime().UnixNano()})
		fmt.Printf("OPT2_DBFILE %s %s\n", phase, raw)
		return nil
	})
	if err != nil {
		t.Fatal("db file inventory")
	}
}
