package serverruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Opt-in measurement only: no credentials or response bodies enter the report.
func TestRemoteMCPTransportMeasure(t *testing.T) {
	if os.Getenv("ASB_REMOTE_MCP_MEASURE") != "1" {
		t.Skip("opt-in isolated HTTP policy measurement")
	}
	dir := t.TempDir()
	s, e := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	if e != nil {
		t.Fatal("seed open")
	}
	if s.Setup(rand.Text()+rand.Text()) != nil || s.CreateProject("probe") != nil || s.SetScopeIssuanceReady("admin", true) != nil {
		t.Fatal("seed setup")
	}
	parent, e := s.IssueProjectTokenWithOptionsFor("admin", "probe", "probe-parent", controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto), UnlimitedAutoBindings: true})
	if e != nil {
		t.Fatal("parent")
	}
	session, e := s.IssueTokenFor("admin", "probe", "probe-session", "admin")
	if e != nil {
		t.Fatal("session")
	}
	if s.AllowHTTP("probe") {
		t.Fatal("HTTP policy unexpectedly on")
	}
	if s.Close() != nil {
		t.Fatal("seed close")
	}
	h, closeRuntime, e := newManagedHandler(dir, nil, "0.17.3")
	if e != nil {
		t.Fatal("runtime")
	}
	defer closeRuntime()
	server := httptest.NewServer(h)
	defer server.Close()
	form := url.Values{"client_id": {"asb-client-v1"}, "harness": {"codex"}, "session_id": {"00000000-0000-0000-0000-000000000099"}, "asb_client_instance_id": {"remote-mcp-probe-instance"}, "request_id": {"probe-request"}}
	cases := []struct{ name, path, token, body, contentType string }{
		{"mcp-session", "/mcp?project=probe", session, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"asb-client","version":"0.17.3"}}}`, "application/json"},
		{"mcp-parent", "/mcp?project=probe", parent, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"asb-client","version":"0.17.3"}}}`, "application/json"},
		{"device", "/oauth/device_authorization?project=probe", parent, form.Encode(), "application/x-www-form-urlencoded"},
		{"direct", "/oauth/session_registration?project=probe", parent, form.Encode(), "application/x-www-form-urlencoded"},
	}
	for _, c := range cases {
		for _, marked := range []bool{false, true} {
			req, e := http.NewRequest(http.MethodPost, server.URL+c.path, strings.NewReader(c.body))
			if e != nil {
				t.Fatal("request")
			}
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Content-Type", c.contentType)
			req.Header.Set("Accept", "application/json, text/event-stream")
			if marked {
				req.Header.Set("X-ASB-Client-Transport", "http")
			}
			out, e := server.Client().Do(req)
			if e != nil {
				t.Fatal("HTTP request")
			}
			raw, e := io.ReadAll(io.LimitReader(out.Body, 65536))
			out.Body.Close()
			if e != nil {
				t.Fatal("HTTP body")
			}
			var body map[string]json.RawMessage
			json.Unmarshal(raw, &body)
			code := ""
			if x := body["error"]; len(x) > 0 && x[0] == '"' {
				json.Unmarshal(x, &code)
			}
			row, _ := json.Marshal(map[string]any{"measurement": "M4", "case": c.name, "markedHTTP": marked, "status": out.StatusCode, "errorCode": code, "tokenReturned": len(body["access_token"]) > 0, "policyOff": true})
			fmt.Println(string(row))
			if bytes.Contains(row, []byte(c.token)) {
				t.Fatal("credential entered summary")
			}
			if marked && (out.StatusCode != http.StatusForbidden || code != "http_not_allowed") {
				t.Fatal("marked request did not enforce policy")
			}
		}
	}
}

// M1 uses the real client and stores only synthetic parent/queue metadata.
func TestRemoteMCPMemoryMeasure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Mac-only RSS/runtime overlay probe")
	}
	if os.Getenv("ASB_REMOTE_MCP_MEASURE") != "1" {
		t.Skip("opt-in client memory measurement")
	}
	root, e := os.MkdirTemp("/private/tmp", "rm")
	if e != nil {
		t.Fatal("temp root")
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	source, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal("source")
	}
	mainPath := filepath.Join(source, "cmd/asb-client/main.go")
	raw, e := os.ReadFile(mainPath)
	if e != nil {
		t.Fatal("client source")
	}
	overlayText := strings.Replace(string(raw), "\"context\"", "\"context\"\n measureJSON \"encoding/json\"\n measureNet \"net\"\n measureRuntime \"runtime\"\n measurePprof \"runtime/pprof\"", 1) + memoryProbeOverlay
	overlayFile := filepath.Join(root, "client-main.go")
	if os.WriteFile(overlayFile, []byte(overlayText), 0600) != nil {
		t.Fatal("overlay")
	}
	overlay, _ := json.Marshal(map[string]any{"Replace": map[string]string{mainPath: overlayFile}})
	overlayJSON := filepath.Join(root, "overlay.json")
	os.WriteFile(overlayJSON, overlay, 0600)
	binary := filepath.Join(root, "asb-client")
	build := exec.Command("go", "build", "-overlay", overlayJSON, "-o", binary, "./cmd/asb-client")
	build.Dir = source
	if build.Run() != nil {
		t.Fatal("build client overlay")
	}
	helperSource := filepath.Join(root, "parent.go")
	os.WriteFile(helperSource, []byte(memoryProbeParent), 0600)
	helper := filepath.Join(root, "codex")
	if exec.Command("go", "build", "-o", helper, helperSource).Run() != nil {
		t.Fatal("build parent helper")
	}
	control, e := controlpanel.OpenSQLite(filepath.Join(root, "data/control"))
	if e != nil {
		t.Fatal("seed")
	}
	if control.Setup(rand.Text()+rand.Text()) != nil || control.CreateProject("probe") != nil || control.SetInstanceHTTPPolicy("admin", true) != nil {
		t.Fatal("setup")
	}
	tokens := make([]string, 3)
	for i := range tokens {
		tokens[i], e = control.IssueTokenFor("admin", "probe", fmt.Sprintf("memory-%d", i), "admin")
		if e != nil {
			t.Fatal("client credential")
		}
	}
	peerToken, e := control.IssueTokenFor("admin", "probe", "peer", "admin")
	if e != nil {
		t.Fatal("peer credential")
	}
	control.Close()
	h, closeRuntime, e := newManagedHandler(filepath.Join(root, "data"), nil, "0.17.3")
	if e != nil {
		t.Fatal("runtime")
	}
	defer closeRuntime()
	server := httptest.NewServer(h)
	defer server.Close()
	peerHTTP := &http.Client{Transport: matrixBearerTransport{token: peerToken}}
	peer, e := mcp.NewClient(&mcp.Implementation{Name: "memory-peer", Version: "0.17.3"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp?project=probe", HTTPClient: peerHTTP}, nil)
	if e != nil {
		t.Fatal("peer connect")
	}
	defer peer.Close()
	call := func(s *mcp.ClientSession, name string, args any, meta mcp.Meta, out any) {
		t.Helper()
		r, e := s.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args, Meta: meta})
		if e != nil || r.IsError {
			t.Fatal("fixture tool", name)
		}
		b, _ := json.Marshal(r.StructuredContent)
		if json.Unmarshal(b, out) != nil {
			t.Fatal("fixture result", name)
		}
	}
	var registered protocol.RegisterResult
	call(peer, "register_session", protocol.RegisterRequest{Harness: "codex", Name: "peer", SessionID: "22222222-2222-4222-8222-222222222222", PID: os.Getpid(), CWD: root, ClientID: "memory-peer-client", ChannelID: "memory-peer", RequestID: "memory-peer", Secret: clientstate.NewSecret()}, nil, &registered)
	if registered.Code != "" {
		t.Fatal("peer register")
	}
	peerCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		tick := time.NewTicker(8 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-peerCtx.Done():
				return
			case <-tick.C:
				peer.CallTool(peerCtx, &mcp.CallToolParams{Name: "renew_lease", Arguments: protocol.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: "memory-peer"}})
			}
		}
	}()
	for i, label := range []string{"default", "limit8-gc50", "limit8-gc25"} {
		home := filepath.Join(root, fmt.Sprint(i))
		cwd := filepath.Join(home, "project")
		state := filepath.Join(home, "state")
		ch := filepath.Join(home, ".codex")
		sid := fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)
		dir := filepath.Join(ch, "sessions/2026/10/30")
		os.MkdirAll(dir, 0700)
		os.MkdirAll(cwd, 0700)
		metaLine, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": sid, "cwd": cwd}})
		os.WriteFile(filepath.Join(dir, "rollout-probe-"+sid+".jsonl"), append(metaLine, '\n'), 0600)
		sock := filepath.Join(root, fmt.Sprintf("p%d.sock", i))
		queue := filepath.Join(root, fmt.Sprintf("q%d", i))
		config, _ := json.Marshal(map[string]string{"binary": binary, "endpoint": server.URL + "/mcp?project=probe", "state": state})
		cmd := exec.Command(helper)
		cmd.Dir = cwd
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home, "CODEX_HOME=" + ch, "TMPDIR=/private/tmp", "ASB_MEMORY_CONFIG=" + string(config), "ASB_MEMORY_TOKEN=" + tokens[i], "ASB_MEMORY_QUEUE=" + queue, "ASB_MEMORY_SOCKET=" + sock}
		if i > 0 {
			cmd.Env = append(cmd.Env, "GOMEMLIMIT=8MiB", fmt.Sprintf("GOGC=%d", []int{0, 50, 25}[i]))
		}
		cmd.Stderr = io.Discard
		local, e := mcp.NewClient(&mcp.Implementation{Name: "synthetic-memory-parent", Version: "1"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
		if e != nil {
			t.Fatal("client initialize")
		}
		t.Cleanup(func() { _ = local.Close() })
		snapshot := func(stage string) map[string]any {
			t.Helper()
			conn, e := net.Dial("unix", sock)
			if e != nil {
				t.Fatal("profile socket")
			}
			defer conn.Close()
			conn.Write([]byte("s"))
			var out map[string]any
			if json.NewDecoder(conn).Decode(&out) != nil {
				t.Fatal("profile decode")
			}
			pid := int(out["pid"].(float64))
			rss, e := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
			if e != nil {
				t.Fatal("RSS")
			}
			kib, e := strconv.Atoi(strings.TrimSpace(string(rss)))
			if e != nil {
				t.Fatal("RSS parse")
			}
			out["rssKiB"] = kib
			out["measurement"] = "M1"
			out["variant"] = label
			out["stage"] = stage
			b, _ := json.Marshal(out)
			fmt.Println(string(b))
			return out
		}
		time.Sleep(time.Second)
		snapshot("idle-unregistered")
		var own protocol.Session
		call(local, "attach_session", map[string]any{}, mcp.Meta{"threadId": sid}, &own)
		if !own.AutomaticDeliveryReady {
			t.Fatal("receiver not ready")
		}
		time.Sleep(time.Second)
		before := snapshot("registered")
		const n = 30
		maxRSS := 0
		latencies := []float64{}
		started := time.Now()
		for seq := 1; seq <= n; seq++ {
			var sent protocol.SendResult
			call(peer, "send_message", protocol.Message{From: registered.Session.Name, To: own.Name, Body: "synthetic-memory-probe"}, nil, &sent)
			if !sent.Accepted {
				t.Fatal("send")
			}
			until := time.Now().Add(10 * time.Second)
			for {
				b, _ := os.ReadFile(queue)
				lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
				if bytes.HasSuffix(b, []byte("\n")) && len(lines) >= seq {
					var q map[string]int64
					if json.Unmarshal(lines[seq-1], &q) != nil {
						t.Fatal("queue metadata")
					}
					latencies = append(latencies, float64(q["at"]-q["sentAt"]))
					break
				}
				if time.Now().After(until) {
					t.Fatal("queue timeout")
				}
				time.Sleep(2 * time.Millisecond)
			}
			if seq == n/2 {
				out := snapshot("receiving")
				maxRSS = out["rssKiB"].(int)
			}
		}
		after := snapshot("after-receive")
		sort.Float64s(latencies)
		summary, _ := json.Marshal(map[string]any{"measurement": "M1", "variant": label, "stage": "burst-summary", "messages": n, "elapsedMs": time.Since(started).Milliseconds(), "sampleReceivingRSSKiB": maxRSS, "latencyMedianMs": latencies[n/2], "latencyMaxMs": latencies[n-1], "clientCPUms": after["cpuMs"].(float64) - before["cpuMs"].(float64), "gcCycles": after["numGC"].(float64) - before["numGC"].(float64)})
		fmt.Println(string(summary))
		if i == 0 {
			conn, e := net.Dial("unix", sock)
			if e != nil {
				t.Fatal("heap socket")
			}
			conn.Write([]byte("h"))
			profile := filepath.Join(root, "heap.pprof")
			f, _ := os.Create(profile)
			_, e = io.Copy(f, conn)
			f.Close()
			conn.Close()
			if e != nil {
				t.Fatal("heap profile")
			}
			top, e := exec.Command("go", "tool", "pprof", "-top", "-nodecount=8", binary, profile).Output()
			if e != nil {
				t.Fatal("pprof")
			}
			fmt.Printf("M1_PPROF\n%s\n", top)
		}
		local.Close()
	}
}

const memoryProbeOverlay = `
func init(){p:=os.Getenv("ASB_MEMORY_SOCKET");if p==""{return};l,e:=measureNet.Listen("unix",p);if e!=nil{panic("profile socket")};os.Chmod(p,0600);go func(){for{c,e:=l.Accept();if e!=nil{return};func(){defer c.Close();var b[1]byte;c.Read(b[:]);if b[0]=='h'{measurePprof.WriteHeapProfile(c);return};var m measureRuntime.MemStats;measureRuntime.ReadMemStats(&m);var r syscall.Rusage;syscall.Getrusage(syscall.RUSAGE_SELF,&r);cpu:=float64(r.Utime.Sec+r.Stime.Sec)*1000+float64(r.Utime.Usec+r.Stime.Usec)/1000;measureJSON.NewEncoder(c).Encode(map[string]any{"pid":os.Getpid(),"cpuMs":cpu,"heapAlloc":m.HeapAlloc,"heapSys":m.HeapSys,"heapInuse":m.HeapInuse,"heapIdle":m.HeapIdle,"heapReleased":m.HeapReleased,"stackInuse":m.StackInuse,"gcSys":m.GCSys,"otherSys":m.OtherSys,"sys":m.Sys,"totalAlloc":m.TotalAlloc,"numGC":m.NumGC,"gcCPUFraction":m.GCCPUFraction,"pauseTotalNs":m.PauseTotalNs})}()}}()}
`

const memoryProbeParent = `package main
import("os";"os/exec";"encoding/json";"time")
func main(){if len(os.Args)>1&&os.Args[1]=="queue"{var m struct{SentAt time.Time};json.Unmarshal([]byte(os.Args[len(os.Args)-1]),&m);b,_:=json.Marshal(map[string]int64{"at":time.Now().UnixMilli(),"sentAt":m.SentAt.UnixMilli()});f,e:=os.OpenFile(os.Getenv("ASB_MEMORY_QUEUE"),os.O_CREATE|os.O_APPEND|os.O_WRONLY,0600);if e!=nil{os.Exit(2)};f.Write(append(b,'\n'));f.Close();return};var c map[string]string;if json.Unmarshal([]byte(os.Getenv("ASB_MEMORY_CONFIG")),&c)!=nil{os.Exit(3)};cmd:=exec.Command(c["binary"],"--broker-url",c["endpoint"],"--token",os.Getenv("ASB_MEMORY_TOKEN"),"--state-dir",c["state"]);cmd.Stdin=os.Stdin;cmd.Stdout=os.Stdout;cmd.Stderr=os.Stderr;if cmd.Run()!=nil{os.Exit(4)}}
`

// Local-only bridge keeps temporary bearer values out of files and stdout.
func TestRemoteMCPHarnessServer(t *testing.T) {
	root := os.Getenv("ASB_REMOTE_MCP_ROOT")
	if root == "" {
		t.Skip("opt-in harness bridge")
	}
	if !strings.HasPrefix(root, "/private/tmp/") {
		t.Fatal("isolated root required")
	}
	seed, e := controlpanel.OpenSQLite(filepath.Join(root, "data/control"))
	if e != nil {
		t.Fatal("seed")
	}
	if seed.Setup(rand.Text()+rand.Text()) != nil || seed.CreateProject("probe") != nil {
		t.Fatal("seed setup")
	}
	parent, e := seed.IssueProjectTokenFor("admin", "probe", "http-probe")
	if e != nil {
		t.Fatal("parent")
	}
	seed.Close()
	h, closeRuntime, e := newManagedHandler(filepath.Join(root, "data"), nil, "0.17.3")
	if e != nil {
		t.Fatal("runtime")
	}
	defer closeRuntime()
	audit := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 65536))
		r.Body = io.NopCloser(bytes.NewReader(b))
		var q map[string]any
		json.Unmarshal(b, &q)
		headers := []string{}
		ids := map[string]string{}
		for k, v := range r.Header {
			headers = append(headers, k)
			key := strings.ToLower(k)
			if strings.Contains(key, "session") || strings.Contains(key, "thread") || strings.Contains(key, "conversation") {
				sum := sha256.Sum256([]byte(strings.Join(v, ",")))
				ids[k] = hex.EncodeToString(sum[:])
			}
		}
		sort.Strings(headers)
		metaKeys := []string{}
		metaIDs := map[string]string{}
		if params, ok := q["params"].(map[string]any); ok {
			if meta, ok := params["_meta"].(map[string]any); ok {
				for k, v := range meta {
					metaKeys = append(metaKeys, k)
					key := strings.ToLower(k)
					if s, ok := v.(string); ok && (strings.Contains(key, "thread") || strings.Contains(key, "session") || strings.Contains(key, "conversation")) {
						sum := sha256.Sum256([]byte(s))
						metaIDs[k] = hex.EncodeToString(sum[:])
					}
				}
			}
		}
		sort.Strings(metaKeys)
		capture := httptest.NewRecorder()
		h.ServeHTTP(capture, r)
		for key, values := range capture.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(capture.Code)
		_, _ = w.Write(capture.Body.Bytes())
		row, _ := json.Marshal(map[string]any{"method": q["method"], "status": capture.Code, "bearerMatchesIssuedParent": r.Header.Get("Authorization") == "Bearer "+parent, "headerNames": headers, "identityHeaderHashes": ids, "metaKeys": metaKeys, "identityMetaHashes": metaIDs})
		f, e := os.OpenFile(filepath.Join(root, "actual-http.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if e == nil {
			f.Write(append(row, '\n'))
			f.Close()
		}
	}
	server := httptest.NewServer(http.HandlerFunc(audit))
	defer server.Close()
	socket := filepath.Join(root, "bridge.sock")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal("bridge")
	}
	defer listener.Close()
	os.Chmod(socket, 0600)
	fmt.Println(`{"measurement":"harness-bridge-ready"}`)
	for {
		c, e := listener.Accept()
		if e != nil {
			t.Fatal("bridge accept")
		}
		var op struct {
			Op string `json:"op"`
		}
		json.NewDecoder(io.LimitReader(c, 1024)).Decode(&op)
		switch op.Op {
		case "config":
			json.NewEncoder(c).Encode(map[string]string{"endpoint": server.URL + "/mcp?project=probe", "token": parent})
		case "stop":
			json.NewEncoder(c).Encode(map[string]bool{"stopped": true})
			c.Close()
			return
		default:
			json.NewEncoder(c).Encode(map[string]string{"error": "unknown-operation"})
		}
		c.Close()
	}
}
