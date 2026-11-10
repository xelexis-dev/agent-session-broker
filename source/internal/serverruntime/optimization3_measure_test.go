//go:build darwin

package serverruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

// Only temporary overlay files change instrumentation; shipped code is untouched.
// shortcut: needles target the current source layout; revalidate counters before using a different one.
func opt3Overlay(t *testing.T, source, root string) string {
	t.Helper()
	path := optOverlay(t, source, root)
	var overlay struct{ Replace map[string]string }
	raw, e := os.ReadFile(path)
	if e != nil || json.Unmarshal(raw, &overlay) != nil {
		t.Fatal("overlay")
	}
	get := func(name string) string {
		p := name
		if v := overlay.Replace[name]; v != "" {
			p = v
		}
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal("overlay source")
		}
		return string(b)
	}
	serial := 0
	put := func(name, body string) {
		serial++
		p := filepath.Join(root, fmt.Sprintf("opt3-%d.go", serial))
		if os.WriteFile(p, []byte(body), 0600) != nil {
			t.Fatal("overlay output")
		}
		overlay.Replace[name] = p
	}
	goroot := filepath.Join(root, "goroot")
	p := filepath.Join(goroot, "src/os/opt_probe.go")
	s := get(p)
	s = strings.Replace(s, `"runtime"`, `"runtime";"time";"strconv"`, 1)
	a := strings.Index(s, "func OptProbeSnapshot()")
	if a < 0 {
		t.Fatal("snapshot signature")
	}
	s = s[:a] + opt3Snapshot + opt3OS
	put(p, s)
	p = filepath.Join(source, "cmd/asb-server/main.go")
	s = get(p)
	s = strings.Replace(s, "if b[0]=='m'{", `if b[0]=='h'{os.OptProbeHold(true)};if b[0]=='u'{os.OptProbeHold(false)};if b[0]=='z'{os.OptProbeReset()};if b[0]=='g'{optPprof.Lookup("goroutine").WriteTo(c,1);return};if b[0]=='t'{var n int64;if optJSON.NewDecoder(c).Decode(&n)==nil{os.OptProbeOffset(n)}};if b[0]=='m'{`, 1)
	put(p, s)
	p = filepath.Join(source, "cmd/asb-client/main.go")
	s = get(p)
	s = strings.Replace(s, "if b[0]=='m'{", `if b[0]=='z'{os.OptProbeReset()};if b[0]=='g'{optPprof.Lookup("goroutine").WriteTo(c,1);return};if b[0]=='m'{`, 1)
	put(p, s)
	p = filepath.Join(source, "internal/serverruntime/run.go")
	s = get(p)
	s = strings.Replace(s, "server := &http.Server{", "listener = opt3Listener{listener}\nserver := &http.Server{", 1)
	s = strings.Replace(s, "next.ServeHTTP(w,r)", `if name=="/mcp/wait_for_message"{w=&opt3GateWriter{ResponseWriter:w}};next.ServeHTTP(w,r)`, 1) + opt3Wire
	put(p, s)
	p = filepath.Join(goroot, "src/net/http/roundtrip.go")
	s = get(p)
	s = strings.Replace(s, "package http", "package http\nimport(optOS \"os\";optJSON \"encoding/json\";optIO \"io\")", 1)
	needle := "return t.roundTrip(req)"
	if !strings.Contains(s, needle) {
		t.Fatal("roundtrip signature")
	}
	s = strings.Replace(s, needle, `name:=req.URL.Path;if req.GetBody!=nil{if c,e:=req.GetBody();e==nil{raw,_:=optIO.ReadAll(c);c.Close();var v struct{Method string;Params struct{Name string}};if optJSON.Unmarshal(raw,&v)==nil{if v.Params.Name!=""{name+="/"+v.Params.Name}else if v.Method!=""{name+="/"+v.Method}}}};optOS.OptProbeHTTP(name);`+needle, 1)
	put(p, s)
	p = filepath.Join(source, "internal/controlpanel/store.go")
	s = get(p)
	s = strings.Replace(s, "import (", "import (\noptRuntime \"runtime\"", 1)
	s = strings.Replace(s, "func (s *Store) commitAudited(candidate state, events []AuditEvent) error {", "func (s *Store) commitAudited(candidate state, events []AuditEvent) error {\nos.OptProbeEvent(\"controlCommit:\"+opt3CommitCaller())", 1)
	s = strings.Replace(s, "data, err := json.Marshal(candidate)", "data, err := json.Marshal(candidate)\nif err==nil{os.OptProbeAdd(\"controlMarshalBytes\",uint64(len(data)))}", 1)
	s = strings.Replace(s, "s.data = candidate", "os.OptProbeAdd(\"controlWriteBytes\",uint64(len(data)))\ns.data = candidate", 1)
	s = strings.Replace(s, "now: time.Now,", "now: os.OptProbeNow,", 1)
	s += `func opt3CommitCaller()string{for skip:=2;skip<8;skip++{pc,_,_,ok:=optRuntime.Caller(skip);if !ok{break};name:=optRuntime.FuncForPC(pc).Name();if name!="github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel.(*Store).commit"&&name!="github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel.(*Store).commitProject"{return name}};return "unknown"}`
	put(p, s)
	p = filepath.Join(source, "internal/broker/remote_durable.go")
	s = get(p)
	s = strings.Replace(s, "func (s *RemoteService) ResumeSession(ctx context.Context, r ResumeRequest) (RegisterResult, error) {", `func (s *RemoteService) ResumeSession(ctx context.Context, r ResumeRequest) (result RegisterResult, returnErr error) {defer func(){if returnErr==nil&&result.Code==""{optOS.OptProbeEvent("resumeOK:"+r.RegistrationID)}}()`, 1)
	put(p, s)
	p = filepath.Join(source, "internal/clientstate/state.go")
	s = get(p)
	s = strings.Replace(s, "return os.Rename(temporary, path)", `err=os.Rename(temporary,path);if err==nil{if rel,e:=filepath.Rel(os.Getenv("OPT_STATE"),path);e==nil&&!strings.HasPrefix(rel,".."){os.OptProbeEvent("stateTarget:"+rel);os.OptProbeAdd("stateTargetBytes:"+rel,uint64(len(data)))}};return err`, 1)
	put(p, s)
	p = filepath.Join(source, "internal/controlpanel/direct_state.go")
	s = get(p)
	s = strings.Replace(s, "func (s *Store) writeAuditBatch(data []byte, events []AuditEvent, projects map[string]Project) error {", `func (s *Store) writeAuditBatch(data []byte, events []AuditEvent, projects map[string]Project) error {os.OptProbeEvent("auditBatchCalls");os.OptProbeAdd("auditBatchStateBytes",uint64(len(data)))`, 1)
	s = strings.Replace(s, "import (", "import (\n\"os\"", 1)
	put(p, s)
	p = filepath.Join(source, "internal/controlpanel/store.go")
	s = strings.Replace(get(p), "import (", "import (\n\"fmt\"", 1) + opt3Control
	put(p, s)
	p = filepath.Join(source, "internal/serverruntime/managed.go")
	s = get(p)
	s = strings.Replace(s, "return wrapped, close, nil", "return opt3Actions(store,reports,wrapped),close,nil", 1)
	s = strings.Replace(s, "import (", "import (\n\"encoding/json\";\"time\"", 1) + opt3ActionsSource
	put(p, s)
	raw, _ = json.Marshal(overlay)
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("overlay map")
	}
	return path
}

const opt3Snapshot = `func OptProbeSnapshot()map[string]uint64{optProbeMu.Lock();v:=map[string]uint64{};for k,n:=range optProbeCounts{v[k]=n};optProbeMu.Unlock();var m runtime.MemStats;runtime.ReadMemStats(&m);v["HeapAlloc"]=m.HeapAlloc;v["HeapInuse"]=m.HeapInuse;v["Sys"]=m.Sys;v["StackInuse"]=m.StackInuse;v["StackSys"]=m.StackSys;v["GCSys"]=m.GCSys;v["Goroutines"]=uint64(runtime.NumGoroutine());return v}
`
const opt3OS = `var opt3Held bool
var opt3Offset int64
func OptProbeReset(){optProbeMu.Lock();optProbeCounts=map[string]uint64{};optProbeMu.Unlock()}
func OptProbeHold(v bool){optProbeMu.Lock();opt3Held=v;optProbeMu.Unlock()}
func OptProbeHeld()bool{optProbeMu.Lock();defer optProbeMu.Unlock();return opt3Held}
func OptProbeOffset(n int64){optProbeMu.Lock();opt3Offset=n;optProbeMu.Unlock()}
func OptProbeNow()time.Time{optProbeMu.Lock();n:=opt3Offset;optProbeMu.Unlock();return time.Now().Add(time.Duration(n)*time.Second)}
func OptProbeHTTP(name string){OptProbeEvent("http:"+name);if Getenv("OPT_TIMING")=="1"{OptProbeEvent("httpAt:"+name+":"+strconv.FormatInt(time.Now().UnixMilli(),10))}}
`
const opt3Wire = `
type opt3Listener struct{net.Listener}
func(l opt3Listener)Accept()(net.Conn,error){c,e:=l.Listener.Accept();if e!=nil{return nil,e};return &opt3Conn{Conn:c},nil}
type opt3Conn struct{net.Conn}
func(c *opt3Conn)Read(p []byte)(int,error){n,e:=c.Conn.Read(p);if n>0{optOS.OptProbeAdd("tcpReadBytes",uint64(n))};return n,e}
func(c *opt3Conn)Write(p []byte)(int,error){n,e:=c.Conn.Write(p);if n>0{optOS.OptProbeAdd("tcpWriteBytes",uint64(n))};return n,e}
type opt3GateWriter struct{http.ResponseWriter}
func(w *opt3GateWriter)Write(p []byte)(int,error){for optOS.OptProbeHeld(){time.Sleep(5*time.Millisecond)};return w.ResponseWriter.Write(p)}
func(w *opt3GateWriter)Flush(){if f,ok:=w.ResponseWriter.(http.Flusher);ok{f.Flush()}}
func(w *opt3GateWriter)Unwrap()http.ResponseWriter{return w.ResponseWriter}
`

// Raw MCP keeps channel notifications visible without involving a model.
type opt3Client struct {
	conn                           mcp.Connection
	mu                             sync.Mutex
	writes                         sync.Mutex
	next                           atomic.Int64
	pending                        map[string]chan json.RawMessage
	events                         chan map[string]json.RawMessage
	done                           chan struct{}
	name, registration, sock, home string
	credential                     string
	thread                         string
}

func (c *opt3Client) write(ctx context.Context, v any) error {
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	m, e := jsonrpc.DecodeMessage(raw)
	if e != nil {
		return e
	}
	c.writes.Lock()
	defer c.writes.Unlock()
	return c.conn.Write(ctx, m)
}
func (c *opt3Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := strconv.FormatInt(c.next.Add(1), 10)
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if e := c.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); e != nil {
		return nil, e
	}
	select {
	case raw := <-ch:
		var v struct {
			Result json.RawMessage
			Error  json.RawMessage
		}
		if json.Unmarshal(raw, &v) != nil || len(v.Error) > 0 {
			return nil, fmt.Errorf("synthetic MCP response error")
		}
		return v.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("synthetic MCP closed")
	}
}
func (c *opt3Client) tool(ctx context.Context, name string, args any) (map[string]json.RawMessage, error) {
	params := map[string]any{"name": name, "arguments": args}
	if c.thread != "" {
		params["_meta"] = map[string]any{"threadId": c.thread}
	}
	raw, e := c.call(ctx, "tools/call", params)
	if e != nil {
		return nil, e
	}

	var out mcp.CallToolResult
	if json.Unmarshal(raw, &out) != nil {
		return nil, fmt.Errorf("synthetic tool decode")
	}
	if out.IsError {
		var hints []string
		for _, part := range out.Content {
			if text, ok := part.(*mcp.TextContent); ok {
				for _, word := range []string{"invalid", "scope", "recipient", "ready", "registered", "compatib", "lease", "unsupported", "authoriz", "caller", "identity", "denied", "activated", "schema", "property", "additional", "argument", "from"} {
					if strings.Contains(strings.ToLower(text.Text), word) {
						hints = append(hints, word)
					}
				}
			}
		}
		safe := ""
		for _, part := range out.Content {
			if text, ok := part.(*mcp.TextContent); ok {
				safe = strings.ReplaceAll(text.Text, c.credential, "<redacted>")
			}
		}
		return nil, fmt.Errorf("synthetic tool failed: %s hints=%s detail=%s", name, strings.Join(hints, ","), safe)
	}
	var value map[string]json.RawMessage
	if out.StructuredContent != nil {
		data, _ := json.Marshal(out.StructuredContent)
		json.Unmarshal(data, &value)
	} else if len(out.Content) > 0 {
		if text, ok := out.Content[0].(*mcp.TextContent); ok {
			json.Unmarshal([]byte(text.Text), &value)
		}
	}
	return value, nil
}

func (c *opt3Client) read(ctx context.Context) {
	defer close(c.done)
	for {
		m, e := c.conn.Read(ctx)
		if e != nil {
			return
		}
		raw, e := jsonrpc.EncodeMessage(m)
		if e != nil {
			return
		}
		var v struct {
			ID     string
			Method string
			Params map[string]json.RawMessage
		}
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		if v.ID != "" {
			c.mu.Lock()
			ch := c.pending[v.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- raw
			}
		} else if v.Method == "notifications/claude/channel" {
			select {
			case c.events <- v.Params:
			case <-ctx.Done():
				return
			}
		}
	}
}

// An isolated fixture owns every spawned process and all data underneath root.
type opt3Fixture struct {
	t                                                       *testing.T
	ctx                                                     context.Context
	cancel                                                  context.CancelFunc
	root, source, overlay, client, server, parent, addr, ss string
	tokens                                                  []string
	srv                                                     *exec.Cmd
	clients                                                 []*opt3Client
}

func opt3New(t *testing.T, n int) *opt3Fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	root, e := os.MkdirTemp("/private/tmp/optm", "o3-")
	if e != nil {
		t.Fatal("root")
	}
	source, _ := filepath.Abs("../..")
	f := &opt3Fixture{t: t, ctx: ctx, cancel: cancel, root: root, source: source, ss: filepath.Join(root, "s.sock")}
	t.Cleanup(func() {
		cancel()
		for _, c := range f.clients {
			c.conn.Close()
			<-c.done
		}
		if f.srv != nil {
			f.srv.Process.Signal(os.Interrupt)
			f.srv.Wait()
		}
		// The copied Go toolchain retains read-only directory modes.
		filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() {
				return os.Chmod(path, 0700)
			}
			return nil
		})
		if e := os.RemoveAll(root); e != nil {
			t.Error("temporary root cleanup failed")
		}
	})
	f.overlay = opt3Overlay(t, source, root)
	for _, kind := range []string{"client", "server"} {
		out := filepath.Join(root, "asb-"+kind)
		cmd := exec.Command("go", "build", "-overlay", f.overlay, "-o", out, "./cmd/asb-"+kind)
		cmd.Dir = source
		cmd.Env = append(os.Environ(), "GOROOT="+filepath.Join(root, "goroot"))
		if raw, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("probe build: %s", raw)
		}
		if kind == "client" {
			f.client = out
		} else {
			f.server = out
		}
	}
	file := filepath.Join(root, "parent.go")
	parentSource := optParent
	if os.Getenv("ASB_OPT3_CODEX") == "1" {
		parentSource = strings.Replace(parentSource, `"os";`, `"os";"net";`, 1)
		parentSource = strings.Replace(parentSource, "func main(){", `func main(){if len(os.Args)>1{if len(os.Args)==6&&os.Args[1]=="queue"{var v struct{Body string};if json.Unmarshal([]byte(os.Args[5]),&v)!=nil{os.Exit(2)};c,e:=net.Dial("unix",os.Getenv("OPT_HANDOFF"));if e!=nil{os.Exit(2)};e=json.NewEncoder(c).Encode(map[string]string{"content":v.Body});c.Close();if e!=nil{os.Exit(2)};return};os.Exit(2)};`, 1)
	}
	os.WriteFile(file, []byte(parentSource), 0600)
	os.MkdirAll(filepath.Join(root, "bin"), 0700)
	kind := "claude"
	if os.Getenv("ASB_OPT3_CODEX") == "1" {
		kind = "codex"
	}
	f.parent = filepath.Join(root, "bin", kind)
	if exec.Command("go", "build", "-o", f.parent, file).Run() != nil {
		t.Fatal("parent build")
	}
	seed, e := controlpanel.OpenSQLite(filepath.Join(root, "data/control"))
	if e != nil {
		t.Fatal("seed")
	}
	if seed.Setup("synthetic-measurement-password") != nil || seed.CreateProject("probe") != nil || seed.SetInstanceHTTPPolicy("admin", true) != nil {
		t.Fatal("setup")
	}

	sessionGrants := os.Getenv("ASB_OPT3_SESSION_GRANTS") == "1"
	parent := ""
	if sessionGrants && seed.SetScopeIssuanceReady("admin", true) != nil {
		t.Fatal("scope setup")
	}
	for i := 0; i < n; i++ {
		token := ""
		if sessionGrants {
			if i%10 == 0 {
				scopes := append(access.LegacyV1Scopes(), access.SessionRegisterAuto)
				parent, e = seed.IssueProjectTokenWithOptionsFor("admin", "probe", fmt.Sprint("parent-", i), controlpanel.ProjectTokenOptions{Scopes: scopes, UnlimitedAutoBindings: true, UnlimitedAutoLifetime: true})
				if e != nil {
					t.Fatal("auto parent")
				}
			}
			home := filepath.Join(root, fmt.Sprint("c", i))
			state, e := clientstate.Open(filepath.Join(home, "state"))
			if e != nil {
				t.Fatal("seed state")
			}
			instance, e := state.ClientID()
			if e != nil {
				t.Fatal("instance")
			}
			harness := "claude"
			if os.Getenv("ASB_OPT3_CODEX") == "1" {
				harness = "codex"
			}
			reply, e := seed.ExchangeDirect("probe", parent, controlpanel.DirectRequest{DeviceRequest: controlpanel.DeviceRequest{ClientID: "asb-client-v1", Harness: harness, SessionID: fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1), InstanceID: instance, RequestID: fmt.Sprint("seed-", i)}, RequestedNickname: fmt.Sprint("auto-", i)}, controlpanel.DirectProof{Absent: true}, fmt.Sprint("synthetic-", i), false)
			if e != nil {
				t.Fatal("seed session grant")
			}
			token = reply.AccessToken
		} else {
			token, e = seed.IssueTokenFor("admin", "probe", fmt.Sprint(i), "admin")
			if e != nil {
				t.Fatal("token seed")
			}
		}
		f.tokens = append(f.tokens, token)
	}

	seed.Close()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal("listen")
	}
	f.addr = l.Addr().String()
	l.Close()
	f.start()
	return f
}
func (f *opt3Fixture) start() {
	f.t.Helper()
	os.Remove(f.ss)
	cmd := exec.Command(f.server, "--listen", f.addr, "--memory-dir", filepath.Join(f.root, "data"))
	cmd.Env = optEnv(f.root, f.ss)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if cmd.Start() != nil {
		f.t.Fatal("server start")
	}
	f.srv = cmd
	until := time.Now().Add(15 * time.Second)
	for {
		r, e := http.Get("http://" + f.addr + "/healthz")
		if e == nil {
			r.Body.Close()
			return
		}
		if time.Now().After(until) {
			f.t.Fatal("server health")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func (f *opt3Fixture) add() *opt3Client {
	f.t.Helper()
	i := len(f.clients)
	if i >= len(f.tokens) {
		f.t.Fatal("fixture credential count")
	}
	home := filepath.Join(f.root, fmt.Sprint("c", i))
	os.MkdirAll(home, 0700)
	sock := filepath.Join(f.root, fmt.Sprintf("c%d.sock", i))
	sid := fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)
	codexMode := os.Getenv("ASB_OPT3_CODEX") == "1"
	if codexMode {
		dir := filepath.Join(home, ".codex/sessions/2026/10/31")
		os.MkdirAll(dir, 0700)
		meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": sid, "cwd": home}})
		for _, kind := range []string{"task_started", "task_complete"} {
			line, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "type": "event_msg", "payload": map[string]string{"type": kind, "turn_id": "synthetic-idle"}})
			meta = append(append(meta, '\n'), line...)
		}
		meta = append(meta, '\n')
		if os.WriteFile(filepath.Join(dir, "rollout-probe-"+sid+".jsonl"), meta, 0600) != nil {
			f.t.Fatal("synthetic rollout")
		}
	}
	cmd := exec.Command(f.parent)
	cmd.Dir = home
	cmd.Env = append(optEnv(home, sock), "OPT_BINARY="+f.client, "OPT_ENDPOINT=http://"+f.addr+"/mcp?project=probe", "OPT_TOKEN="+f.tokens[i], "OPT_STATE="+filepath.Join(home, "state"), "CLAUDE_CODE_SESSION_ID="+sid, "OPT_TIMING=1")
	if codexMode {
		cmd.Env = append(cmd.Env, "CODEX_THREAD_ID="+sid, "OPT_HANDOFF="+filepath.Join(home, "handoff.sock"))
	}
	cmd.Stderr = io.Discard
	conn, e := (&mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}).Connect(f.ctx)
	if e != nil {
		f.t.Fatal("client process")
	}
	c := &opt3Client{credential: f.tokens[i], conn: conn, pending: map[string]chan json.RawMessage{}, events: make(chan map[string]json.RawMessage, 10000), done: make(chan struct{}), sock: sock, home: home}
	f.clients = append(f.clients, c)
	go c.read(f.ctx)
	if _, e = c.call(f.ctx, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "synthetic", "version": "1"}}); e != nil {
		f.t.Fatal("initialize")
	}
	if c.write(f.ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}) != nil {
		f.t.Fatal("initialized")
	}
	if codexMode {
		c.thread = sid
		l, e := net.Listen("unix", filepath.Join(home, "handoff.sock"))
		if e != nil {
			f.t.Fatal("handoff socket")
		}
		f.t.Cleanup(func() { l.Close() })
		go func() {
			for {
				conn, e := l.Accept()
				if e != nil {
					return
				}
				var v map[string]json.RawMessage
				e = json.NewDecoder(conn).Decode(&v)
				conn.Close()
				if e == nil {
					select {
					case c.events <- v:
					case <-f.ctx.Done():
						return
					}
				}
			}
		}()
	} else {
		var event map[string]json.RawMessage
		select {
		case event = <-c.events:
		case <-time.After(10 * time.Second):
			f.t.Fatal("activation event")
		}
		var meta map[string]string
		json.Unmarshal(event["meta"], &meta)
		if meta["activation"] == "" {
			f.t.Fatal("activation token")
		}
		if _, e = c.tool(f.ctx, "activate_channel", map[string]any{"token": meta["activation"]}); e != nil {
			f.t.Fatal("activate")
		}
	}
	out, e := c.tool(f.ctx, "attach_session", map[string]any{})
	if e != nil {
		f.t.Fatal("attach")
	}
	json.Unmarshal(out["name"], &c.name)
	json.Unmarshal(out["registrationId"], &c.registration)
	if c.name == "" || c.registration == "" {
		f.t.Fatal("attached identity")
	}
	return c
}
func opt3Delta(a, b map[string]any) map[string]float64 {
	d := map[string]float64{}
	for k, v := range b {
		if n, ok := v.(float64); ok {
			prior, _ := a[k].(float64)
			if n != prior && !strings.HasPrefix(k, "httpAt:") {
				d[k] = n - prior
			}
		}
	}
	return d
}
func opt3Print(label string, v any) {
	raw, _ := json.Marshal(v)
	fmt.Printf("OPT3 %s %s\n", label, raw)
}
func opt3RSS(t *testing.T, pid int) int {
	t.Helper()
	out, e := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if e != nil {
		t.Fatal("RSS")
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func TestOptimization3Measure(t *testing.T) {
	mode := os.Getenv("ASB_OPT3_MEASURE")
	if mode == "" {
		t.Skip("opt-in isolated OPT3 measurement")
	}
	if os.MkdirAll("/private/tmp/optm", 0700) != nil {
		t.Fatal("tmp")
	}
	switch mode {
	case "M1":
		f := opt3New(t, 11)
		for len(f.clients) < 11 {
			f.add()
		}
		time.Sleep(time.Second)
		count := 200
		if os.Getenv("ASB_OPT_SMOKE") == "1" {
			count = 5
		}
		opt3Messages(t, f, count, 1)
		opt3Messages(t, f, count, 10)
	case "M3":
		opt3Growth(t)
	case "M2":
		f := opt3New(t, 30)
		for len(f.clients) < 30 {
			f.add()
		}
		time.Sleep(2 * time.Second)
		opt3Restart(t, f, 0)
		opt3Restart(t, f, 20*time.Second)
	case "M5":
		f := opt3New(t, 30)
		for len(f.clients) < 30 {
			f.add()
		}
		time.Sleep(15 * time.Second)
		duration := time.Hour
		if os.Getenv("ASB_OPT_SMOKE") == "1" {
			duration = 5 * time.Second
		}
		before := optSample(t, f.ss, "s")
		opt3Print("M5-BEGIN", map[string]any{"seconds": duration.Seconds(), "sessionGrants": os.Getenv("ASB_OPT3_SESSION_GRANTS") == "1", "root": f.root})
		until := time.Now().Add(duration)
		for time.Now().Before(until) {
			if _, err := os.Stat(filepath.Join(f.root, "stop")); err == nil {
				t.Log("M5 aborted with cleanup")
				return
			}
			time.Sleep(min(time.Second, time.Until(until)))
		}
		after := optSample(t, f.ss, "s")
		opt3Print("M5-END", map[string]any{"seconds": duration.Seconds(), "server": opt3Delta(before, after)})
	case "M4":
		f := opt3New(t, 60)
		for _, n := range []int{0, 10, 30, 60} {
			for len(f.clients) < n {
				f.add()
			}
			time.Sleep(2 * time.Second)
			v := optSample(t, f.ss, "s")
			v["rssKiB"] = opt3RSS(t, f.srv.Process.Pid)
			opt3Print(fmt.Sprintf("M4-clients%d", n), v)
			if n == 30 {
				conn, e := net.Dial("unix", f.ss)
				if e != nil {
					t.Fatal("profile")
				}
				conn.Write([]byte("g"))
				raw, _ := io.ReadAll(conn)
				conn.Close()
				file := filepath.Join(f.root, "goroutines.txt")
				os.WriteFile(file, raw, 0600)
				for _, group := range strings.Split(string(raw), "\n\n") {
					lines := strings.Split(group, "\n")
					if len(lines) == 0 {
						continue
					}
					fields := strings.Fields(lines[0])
					if len(fields) == 0 {
						continue
					}
					count, e := strconv.Atoi(fields[0])
					if e != nil {
						continue
					}
					frames := []string{}
					for _, line := range lines[1:] {
						f := strings.Fields(line)
						if len(f) > 2 && f[0] == "#" {
							frames = append(frames, f[2])
						}
					}
					opt3Print("M4-goroutine-group", map[string]any{"count": count, "frames": frames})
				}
			}
		}
	default:
		t.Fatal("unknown OPT3 mode")
	}
}
func opt3Messages(t *testing.T, f *opt3Fixture, n, concurrency int) {
	t.Helper()
	recipient := f.clients[1]
	senders := []*opt3Client{f.clients[0]}
	if concurrency > 1 {
		senders = append(senders, f.clients[2:11]...)
	}
	before := optSample(t, f.ss, "s")
	clientBefore := optSample(t, recipient.sock, "s")
	senderBefore := make([]map[string]any, len(senders))
	for i, c := range senders {
		senderBefore[i] = optSample(t, c.sock, "s")
	}
	var sequence atomic.Int64
	starts := sync.Map{}
	handoffs := sync.Map{}
	latencies := []float64{}
	var receiveErr atomic.Bool
	var sendErr atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			select {
			case event := <-recipient.events:
				var body string
				json.Unmarshal(event["content"], &body)
				if len(body) < 8 {
					receiveErr.Store(true)
					return
				}
				id, e := strconv.Atoi(strings.TrimSpace(body[:8]))
				if e != nil {
					receiveErr.Store(true)
					return
				}
				v, ok := starts.LoadAndDelete(id)
				if !ok {
					receiveErr.Store(true)
					return
				}
				latencies = append(latencies, float64(time.Since(v.(time.Time)).Microseconds())/1000)
				if ch, ok := handoffs.LoadAndDelete(id); ok {
					close(ch.(chan struct{}))
				}
			case <-f.ctx.Done():
				receiveErr.Store(true)
				return
			case <-time.After(20 * time.Second):
				receiveErr.Store(true)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		sender := senders[i%len(senders)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				index := int(sequence.Add(1))
				if index > n {
					return
				}
				body := fmt.Sprintf("%08d", index) + strings.Repeat("x", 92)
				handed := make(chan struct{})
				handoffs.Store(index, handed)
				starts.Store(index, time.Now())
				if _, e := sender.tool(f.ctx, "send_message", map[string]any{"from": sender.name, "to": recipient.name, "expectRegistrationId": recipient.registration, "body": body}); e != nil {
					sendErr.Store(true)
					t.Error(e)
					return
				}
				select {
				case <-handed:
				case <-time.After(20 * time.Second):
					sendErr.Store(true)
					t.Error("handoff timed out")
					return
				case <-f.ctx.Done():
					return
				}
			}
		}()
	}
	wg.Wait()
	if sendErr.Load() {
		f.cancel()
	}
	<-done
	if receiveErr.Load() || len(latencies) != n {
		t.Fatal("handoff mismatch")
	}
	sort.Float64s(latencies)
	time.Sleep(250 * time.Millisecond)
	after := optSample(t, f.ss, "s")
	clientAfter := optSample(t, recipient.sock, "s")
	senderWrites := map[string]float64{}
	for i, c := range senders {
		delta := opt3Delta(senderBefore[i], optSample(t, c.sock, "s"))
		for k, v := range delta {
			if strings.HasPrefix(k, "state") {
				senderWrites[k] += v
			}
		}
	}
	opt3Print(fmt.Sprintf("M1-n%d-concurrent%d", n, concurrency), map[string]any{"p50ms": latencies[(n-1)/2], "p95ms": latencies[(n*95-1)/100], "delivered": len(latencies), "server": opt3Delta(before, after), "recipient": opt3Delta(clientBefore, clientAfter), "sendersStateWrites": senderWrites})
}

// A private response gate establishes queue acceptance without harness handoff.
func opt3Command(t *testing.T, sock, command string) {
	t.Helper()
	c, e := net.Dial("unix", sock)
	if e != nil {
		t.Fatal("probe command")
	}
	defer c.Close()
	c.Write([]byte(command))
	io.Copy(io.Discard, c)
}
func opt3Restart(t *testing.T, f *opt3Fixture, outage time.Duration) {
	t.Helper()
	receiver, sender := f.clients[0], f.clients[1]
	opt3Command(t, f.ss, "h")

	for i := 0; i < 10; i++ {
		result, e := sender.tool(f.ctx, "send_message", map[string]any{"from": sender.name, "to": receiver.name, "expectRegistrationId": receiver.registration, "body": fmt.Sprint("queued-", i)})
		if e != nil {
			t.Fatal(e)
		}
		var accepted bool
		json.Unmarshal(result["accepted"], &accepted)
		if !accepted {
			t.Fatal("pre-restart send was not accepted")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if len(receiver.events) != 0 {
		t.Fatal("queue gate failed")
	}
	before := make([]map[string]any, len(f.clients))
	for i, c := range f.clients {
		before[i] = optSample(t, c.sock, "s")
	}
	killed := time.Now()
	f.srv.Process.Kill()
	f.srv.Wait()
	f.srv = nil
	time.Sleep(outage)
	restarted := time.Now()
	f.start()
	peakRSS := 0
	peakCPU := 0.0
	lastCPU := 0.0
	lastAt := time.Now()
	deadline := time.Now().Add(3 * time.Minute)
	var ready map[string]any
	for {
		v := optSample(t, f.ss, "s")
		count := 0
		for k := range v {
			if strings.HasPrefix(k, "resumeOK:") {
				count++
			}
		}
		rss := opt3RSS(t, f.srv.Process.Pid)
		peakRSS = max(peakRSS, rss)
		cpu, _ := v["cpuSeconds"].(float64)
		now := time.Now()
		if lastCPU > 0 {
			peakCPU = max(peakCPU, (cpu-lastCPU)/now.Sub(lastAt).Seconds()*100)
		}
		lastCPU = cpu
		lastAt = now
		steady := count == 30
		if steady {
			for i, c := range f.clients {
				current := optSample(t, c.sock, "s")
				n, _ := current["http:/mcp/wait_for_message"].(float64)
				old, _ := before[i]["http:/mcp/wait_for_message"].(float64)
				if n <= old {
					steady = false
					break
				}
			}
		}
		if steady {
			ready = v
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume timeout: %d/30", count)
		}
		time.Sleep(100 * time.Millisecond)
	}
	recovered := time.Now()
	time.Sleep(time.Second)
	perClient := []any{}
	first := []int64{}
	for i, c := range f.clients {
		after := optSample(t, c.sock, "s")
		counts := map[string]float64{}
		attempts := []int64{}
		timings := map[string][]int64{}
		for k, v := range after {
			n, ok := v.(float64)
			if !ok {
				continue
			}
			old, _ := before[i][k].(float64)
			if n <= old {
				continue
			}
			if strings.HasPrefix(k, "http:") {
				counts[k] = n - old
			}
			if strings.HasPrefix(k, "httpAt:") {
				at := strings.LastIndex(k, ":")
				ms, _ := strconv.ParseInt(k[at+1:], 10, 64)
				if ms >= killed.UnixMilli() {
					path := k[len("httpAt:"):at]
					for j := 0; j < int(n-old); j++ {
						timings[path] = append(timings[path], ms-killed.UnixMilli())
					}
				}
			}
			if strings.HasPrefix(k, "httpAt:/mcp/resume_session:") {
				ms, _ := strconv.ParseInt(k[strings.LastIndex(k, ":")+1:], 10, 64)
				attempts = append(attempts, ms-killed.UnixMilli())
			}
		}
		sort.Slice(attempts, func(i, j int) bool { return attempts[i] < attempts[j] })
		if len(attempts) > 0 {
			first = append(first, attempts[0])
		}
		for path := range timings {
			sort.Slice(timings[path], func(i, j int) bool { return timings[path][i] < timings[path][j] })
		}
		perClient = append(perClient, map[string]any{"client": i, "requests": counts, "resumeAttemptsMsFromKill": attempts, "attemptsMsFromKill": timings})
	}
	if _, e := sender.tool(f.ctx, "send_message", map[string]any{"from": sender.name, "to": receiver.name, "expectRegistrationId": receiver.registration, "body": "post-restart-marker"}); e != nil {
		t.Fatal(e)
	}
	delivered := map[string]int{}
	marker := false
	until := time.After(10 * time.Second)
	for !marker {
		select {
		case event := <-receiver.events:
			var body string
			json.Unmarshal(event["content"], &body)
			if body == "post-restart-marker" {
				marker = true
			} else {
				delivered[body]++
			}
		case <-until:
			t.Fatal("post restart handoff")
		}
	}
	duplicates := 0
	for _, n := range delivered {
		duplicates += max(0, n-1)
	}
	sort.Slice(first, func(i, j int) bool { return first[i] < first[j] })
	opt3Print(fmt.Sprint("M2-outage", int(outage.Seconds())), map[string]any{"allSteadySecondsAfterStart": recovered.Sub(restarted).Seconds(), "totalSeconds": recovered.Sub(killed).Seconds(), "peakRSSKiB": peakRSS, "sampledPeakCPUPercent": peakCPU, "server": ready, "clients": perClient, "firstResumeSpreadMs": first, "queuedAccepted": 10, "queuedDelivered": len(delivered), "queuedLost": 10 - len(delivered), "queuedDuplicates": duplicates, "postRestartMarker": marker})
}

// These actions exist only in the disposable loopback measurement executable.
const opt3Control = `
func(s *Store)Opt3Counts()map[string]int{s.mu.RLock();defer s.mu.RUnlock();p:=s.data.Projects["probe"];return map[string]int{"audits":len(s.audits),"tokens":len(p.Tokens),"devices":len(p.Devices),"autoBindings":len(p.AutoBindings),"autoExchanges":len(p.AutoExchanges),"directRates":len(s.directRates),"devicePoll":len(s.devicePoll),"tokenChanges":len(s.tokenChanges),"operations":len(s.operations),"changes":len(s.changes),"deviceDirectoryHints":len(s.deviceDirectoryHints)}}
func(s *Store)Opt3Devices()error{raw,e:=s.IssueProjectTokenFor("admin","probe","synthetic devices");if e!=nil{return e};for i:=0;i<10;i++{name:=fmt.Sprint("device-",i);req:=DeviceRequest{ClientID:"asb-client-v1",Harness:"claude",SessionID:name,InstanceID:"synthetic",RequestID:name};code,e:=s.BeginDeviceAuthorization("probe",raw,req);if e!=nil{return e};if _,e=s.ApproveDeviceAuthorization("admin",code.UserCode,name,false);e!=nil{return e};if _,e=s.ExchangeDeviceToken("probe",raw,code.DeviceCode,req.ClientID,false);e!=nil{return e}};return nil}
`
const opt3ActionsSource = `
func opt3Actions(store *controlpanel.Store,reports *bugreport.Store,next http.Handler)http.Handler{return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.URL.Path!="/__opt3"{next.ServeHTTP(w,r);return};var err error;switch r.URL.Query().Get("op"){case "counts":case "audits":for i:=0;i<1000;i++{err=store.RecordSessionManagement("probe","admin",protocol.ManagementEvent{Kind:"detached",ActorName:"synthetic",TargetName:"synthetic",TargetRegistrationID:"11111111-1111-4111-8111-111111111111",At:time.Now().UTC()});if err!=nil{break}};case "devices":err=store.Opt3Devices();case "delete-bugs":var rows []bugreport.Report;rows,_,err=reports.List(bugreport.Filter{},1);if err==nil{for _,row:=range rows{var report bugreport.Report;report,err=reports.Get(row.ID);if err==nil{_,err=reports.Delete("admin",row.ID,report.ExpectedVersion)};if err!=nil{break}}};default:w.WriteHeader(400);return};if err!=nil{w.WriteHeader(500);return};json.NewEncoder(w).Encode(store.Opt3Counts())})}
`

func opt3Action(t *testing.T, f *opt3Fixture, op string) map[string]int {
	t.Helper()
	r, e := http.Get("http://" + f.addr + "/__opt3?op=" + op)
	if e != nil {
		t.Fatal("fixture action")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("fixture action %s status %d", op, r.StatusCode)
	}
	v := map[string]int{}
	if json.NewDecoder(r.Body).Decode(&v) != nil {
		t.Fatal("fixture count decode")
	}
	return v
}
func opt3Inventory(t *testing.T, f *opt3Fixture, label string, before map[string]any) {
	t.Helper()
	files := map[string]int64{}
	records := map[string]int{}
	clients := map[string]int64{"files": 0, "bytes": 0}
	memory := 0
	e := filepath.WalkDir(f.root, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(f.root, path)
		if strings.HasPrefix(rel, "data/") {
			info, e := d.Info()
			if e != nil {
				return e
			}
			files[rel] = info.Size()
			if strings.HasSuffix(rel, ".md") {
				memory++
			}
			if strings.HasSuffix(rel, ".db") {
				db, e := sql.Open("sqlite", "file:"+path+"?mode=ro")
				if e != nil {
					return e
				}
				var count int
				e = db.QueryRow("SELECT COUNT(*) FROM records").Scan(&count)
				db.Close()
				if e != nil {
					return e
				}
				records[rel] = count
			}
		} else if strings.HasPrefix(rel, "c") && strings.Contains(rel, "/state/") {
			info, e := d.Info()
			if e != nil {
				return e
			}
			clients["files"]++
			clients["bytes"] += info.Size()
		}
		return nil
	})
	if e != nil {
		t.Fatal("inventory")
	}
	after := optSample(t, f.ss, "s")
	opt3Print("M3-"+label, map[string]any{"files": files, "records": records, "memoryNodes": memory, "clientState": clients, "control": opt3Action(t, f, "counts"), "server": after, "operationDelta": opt3Delta(before, after)})
}
func opt3Growth(t *testing.T) {
	f := opt3New(t, 207)
	f.add()
	f.add()
	base := optSample(t, f.ss, "s")
	opt3Inventory(t, f, "initial", base)
	for i := 0; i < 200; i++ {
		c := f.add()
		if _, e := f.clients[0].tool(f.ctx, "retire_session", map[string]any{"registrationId": c.registration}); e != nil {
			t.Fatal(e)
		}
		c.conn.Close()
		<-c.done
	}
	opt3Inventory(t, f, "register-retire200", base)
	base = optSample(t, f.ss, "s")
	opt3Messages(t, f, 5000, 1)
	opt3Inventory(t, f, "messages5000", base)
	base = optSample(t, f.ss, "s")
	for i := 0; i < 5; i++ {
		c := f.add()
		for j := 0; j < 10; j++ {
			if _, e := c.tool(f.ctx, "report_bug", map[string]any{"title": fmt.Sprint("synthetic-", i, "-", j), "summary": "isolated growth fixture"}); e != nil {
				t.Fatal(e)
			}
		}
	}
	opt3Inventory(t, f, "bugs50", base)
	base = optSample(t, f.ss, "s")
	opt3Action(t, f, "delete-bugs")
	opt3Inventory(t, f, "delete-bugs50", base)
	base = optSample(t, f.ss, "s")
	for i := 0; i < 500; i++ {
		if _, e := f.clients[0].tool(f.ctx, "memory_put", map[string]any{"title": fmt.Sprint("synthetic-", i), "body": "isolated growth fixture", "created_by": "synthetic"}); e != nil {
			t.Fatal(e)
		}
	}
	opt3Inventory(t, f, "memory500", base)
	base = optSample(t, f.ss, "s")
	opt3Action(t, f, "audits")
	opt3Inventory(t, f, "audits1000", base)
	base = optSample(t, f.ss, "s")
	opt3Action(t, f, "devices")
	opt3Inventory(t, f, "device-logins10", base)
}
