package serverruntime

// Opt-in binary integration harness. Not linked into server/client artifacts.
// ASB_PHASE6_CLIENT points to a develop-built asb-client. The only synthetic
// host component is a no-model Codex relay with local session metadata/queue.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

const phase6Relay = `package main
import("os";"os/exec")
func main(){
 if len(os.Args)>1 && os.Args[1]=="queue" { for i,a:=range os.Args {if a=="--message" && i+1<len(os.Args){if os.WriteFile(os.Getenv("PHASE6_QUEUE"),[]byte(os.Args[i+1]),0600)!=nil{os.Exit(2)};return}};os.Exit(2) }
 c:=exec.Command(os.Args[1],os.Args[2:]...);c.Stdin=os.Stdin;c.Stdout=os.Stdout;c.Stderr=os.Stderr
 if c.Run()!=nil {os.Exit(1)}
}`

type phase6Counts struct {
	mu    sync.Mutex
	calls map[string]int
	proof bool
	codes []int
	retry []string
	times []time.Time
}

func (c *phase6Counts) n(key string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls[key] }
func (c *phase6Counts) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if r.Method == "POST" && r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if key == "/mcp" {
				var v struct{ Params struct{ Name string } }
				_ = json.Unmarshal(body, &v)
				if v.Params.Name != "" {
					key = v.Params.Name
				}
			}
			if key == "/oauth/session_registration" {
				v, _ := url.ParseQuery(string(body))
				c.mu.Lock()
				c.proof = c.proof || (v.Get("registration_id") != "" && v.Get("resume_secret") != "")
				c.mu.Unlock()
			}
		}
		c.mu.Lock()
		c.calls[key]++
		if r.URL.Path == "/mcp" {
			c.calls[key+"\x00"+r.Header.Get("Authorization")]++
		}
		c.mu.Unlock()
		if key == "/oauth/session_registration" {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			c.mu.Lock()
			c.codes = append(c.codes, rec.Code)
			c.times = append(c.times, time.Now())
			c.retry = append(c.retry, rec.Header().Get("Retry-After"))
			c.mu.Unlock()
			for k, vs := range rec.Header() {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		h.ServeHTTP(w, r)
	})
}

type phase6Runtime struct {
	store   *controlpanel.Store
	server  *httptest.Server
	counts  *phase6Counts
	factory *mcpserver.ProjectFactory
	offset  atomic.Int64
}

// Test-only reflection controls the synthetic clock without a production clock override.
// Fields are set before any server goroutine starts; offset itself is atomic.
func phase6Set(store *controlpanel.Store, name string, value any) {
	f := reflect.ValueOf(store).Elem().FieldByName(name)
	reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Set(reflect.ValueOf(value))
}
func phase6Server(t *testing.T, gate bool) *phase6Runtime {
	t.Helper()
	dir := t.TempDir()
	s, e := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	if e != nil {
		t.Fatal("fixture store open failed")
	}
	r := &phase6Runtime{store: s, counts: &phase6Counts{calls: map[string]int{}}}
	phase6Set(s, "now", func() time.Time { return time.Now().Add(time.Duration(r.offset.Load())) })
	if s.Setup("synthetic-phase6-password") != nil || s.CreateProject("test") != nil || s.SetInstanceHTTPPolicy("admin", true) != nil {
		t.Fatal("fixture policy setup failed")
	}
	if s.SetScopeIssuanceReady("admin", gate) != nil {
		t.Fatal("fixture scope policy setup failed")
	}
	registry, e := groups.NewRegistry(filepath.Join(dir, "projects"))
	if e != nil {
		t.Fatal("fixture registry failed")
	}
	r.factory = mcpserver.NewManagedProjectFactory(registry, persistentBackend)
	h := controlpanel.New(s, panelBackend{factory: r.factory, registry: registry, store: s}, mcpserver.ProjectHandler(r.factory))
	r.server = httptest.NewServer(r.counts.wrap(h))
	t.Cleanup(func() { r.server.Close(); _ = r.factory.Close(); _ = registry.Close(); _ = s.Close() })
	return r
}
func (r *phase6Runtime) parent(t *testing.T, scopes []string, cap int) string {
	t.Helper()
	raw, e := r.store.IssueProjectTokenWithOptionsFor("admin", "test", "fixture", controlpanel.ProjectTokenOptions{Scopes: scopes, MaxAutoBindings: cap})
	if e != nil {
		t.Fatal("fixture parent issuance failed")
	}
	return raw
}

type phase6Client struct {
	session     *mcp.ClientSession
	auth, queue string
	cancel      context.CancelFunc
}

func phase6BuildRelay(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	_ = os.WriteFile(src, []byte(phase6Relay), 0600)
	out := filepath.Join(dir, "codex")
	if runtime.GOOS == "windows" {
		out += ".exe" // the client recognizes the Windows Codex parent by image name
	}
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if cmd.Run() != nil {
		t.Fatal("synthetic host relay build failed")
	}
	return out
}
func phase6Start(t *testing.T, relay, endpoint, parent, nickname, auth, id string) *phase6Client {
	t.Helper()
	if auth == "" {
		auth = t.TempDir()
	}
	home := t.TempDir()
	cwd := t.TempDir()
	sessions := filepath.Join(home, "sessions", "2026", "10", "14")
	_ = os.MkdirAll(sessions, 0700)
	data, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": id, "cwd": cwd}})
	_ = os.WriteFile(filepath.Join(sessions, "rollout-fixture-"+id+".jsonl"), append(data, '\n'), 0600)
	ctx, cancel := context.WithCancel(t.Context())
	queue := filepath.Join(home, "queue.json")
	cmd := exec.Command(relay, os.Getenv("ASB_PHASE6_CLIENT"), "--broker-url", endpoint+"/mcp?project=test", "--token", parent, "--auth-dir", auth, "--nickname", nickname)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home, "CODEX_THREAD_ID="+id, "PHASE6_QUEUE="+queue)
	cmd.Stderr = io.Discard
	session, e := mcp.NewClient(&mcp.Implementation{Name: "phase6-scripted-harness", Version: "test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if e != nil {
		cancel()
		t.Fatal("stdio client connect failed")
	}
	c := &phase6Client{session: session, auth: auth, queue: queue, cancel: cancel}
	t.Cleanup(c.close)
	return c
}
func (c *phase6Client) close() {
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
	c.cancel()
}
func (c *phase6Client) call(t *testing.T, name string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	r, e := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if e != nil {
		t.Fatalf("stdio tool %s transport failed", name)
	}
	return r
}
func phase6Decode[T any](t *testing.T, r *mcp.CallToolResult) T {
	t.Helper()
	if r.IsError {
		t.Fatal("tool returned an error (payload withheld)")
	}
	data, _ := json.Marshal(r.StructuredContent)
	if r.StructuredContent == nil {
		if len(r.Content) == 0 {
			t.Fatal("missing tool result")
		}
		data = []byte(r.Content[0].(*mcp.TextContent).Text)
	}
	var v T
	if json.Unmarshal(data, &v) != nil {
		t.Fatal("tool result decode failed")
	}
	return v
}
func phase6Wait(t *testing.T, label string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal(label + " watchdog expired")
}
func (c *phase6Client) status(t *testing.T) map[string]any {
	return phase6Decode[map[string]any](t, c.call(t, "auth_status", struct{}{}))
}
func (c *phase6Client) registered(t *testing.T) map[string]any {
	t.Helper()
	var s map[string]any
	phase6Wait(t, "registration", func() bool { s = c.status(t); return s["registered"] == true })
	return s
}
func phase6AutoScopes() []string { return append(access.LegacyV1Scopes(), access.SessionRegisterAuto) }
func phase6Saved(t *testing.T, c *phase6Client) (string, clientstate.SessionState) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(c.auth, "sessions", "*", "conn.json"))
	if len(files) != 1 {
		t.Fatal("fixture session state missing")
	}
	data, _ := os.ReadFile(files[0])
	var state clientstate.SessionState
	if json.Unmarshal(data, &state) != nil {
		t.Fatal("fixture session decode failed")
	}
	return files[0], state
}

const phase6OldSeed = `package main
import("os";"path/filepath";"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel")
func main(){d:=os.Args[1];s,e:=controlpanel.OpenSQLite(filepath.Join(d,"control"));if e!=nil{os.Exit(1)};defer s.Close();if s.Setup("synthetic-phase6-password")!=nil||s.CreateProject("test")!=nil||s.SetInstanceHTTPPolicy("admin",true)!=nil{os.Exit(2)};token,e:=s.IssueProjectTokenFor("admin","test","fixture");if e!=nil{os.Exit(3)};if os.WriteFile(filepath.Join(d,"seed-parent"),[]byte(token),0600)!=nil{os.Exit(4)}}`

func phase6OldServer(t *testing.T) (*phase6Runtime, string) {
	t.Helper()
	source := os.Getenv("ASB_PHASE6_OLD_SOURCE")
	binary := os.Getenv("ASB_PHASE6_OLD_SERVER")
	if source == "" || binary == "" {
		t.Fatal("real 0.14.0 matrix requires ASB_PHASE6_OLD_SOURCE and ASB_PHASE6_OLD_SERVER")
	}
	seedDir := filepath.Join(source, "source", "cmd", "phase6seed")
	if os.MkdirAll(seedDir, 0700) != nil {
		t.Fatal("old seeder directory failed")
	}
	if os.WriteFile(filepath.Join(seedDir, "main.go"), []byte(phase6OldSeed), 0600) != nil {
		t.Fatal("old seeder source failed")
	}
	seed := filepath.Join(t.TempDir(), "seed")
	build := exec.Command("go", "build", "-o", seed, "./cmd/phase6seed")
	build.Dir = filepath.Join(source, "source")
	if build.Run() != nil {
		t.Fatal("old-format synthetic fixture builder failed")
	}
	dir := t.TempDir()
	if exec.Command(seed, dir).Run() != nil {
		t.Fatal("old-format synthetic fixture setup failed")
	}
	token, e := os.ReadFile(filepath.Join(dir, "seed-parent"))
	if e != nil {
		t.Fatal("synthetic parent unavailable")
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal("isolated port unavailable")
	}
	address := listener.Addr().String()
	_ = listener.Close()
	cmd := exec.Command(binary, "--listen", address, "--memory-dir", dir)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if cmd.Start() != nil {
		t.Fatal("real old server failed to launch")
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	phase6Wait(t, "old server health", func() bool {
		resp, e := http.Get("http://" + address + "/healthz")
		if e != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200
	})
	target, _ := url.Parse("http://" + address)
	r := &phase6Runtime{counts: &phase6Counts{calls: map[string]int{}}}
	r.server = httptest.NewServer(r.counts.wrap(httputil.NewSingleHostReverseProxy(target)))
	t.Cleanup(r.server.Close)
	return r, string(token)
}
func phase6Manual(t *testing.T, c *phase6Client, endpoint, nickname string) {
	t.Helper()
	var status map[string]any
	phase6Wait(t, "manual authorization pending", func() bool {
		status = c.status(t)
		if status["status"] == "failed" || status["status"] == "rejected" {
			t.Fatal("new client failed before manual approval (fixed status only)")
		}
		return status["status"] == "pending"
	})
	code, _ := status["userCode"].(string)
	if code == "" {
		t.Fatal("pending authorization omitted code")
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	csrfRE := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	getCSRF := func(path string) string {
		resp, e := browser.Get(endpoint + path)
		if e != nil {
			t.Fatal("fixture browser GET failed")
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		matches := csrfRE.FindSubmatch(data)
		if len(matches) != 2 {
			t.Fatal("fixture browser CSRF missing")
		}
		return string(matches[1])
	}
	csrf := getCSRF("/login")
	resp, e := browser.PostForm(endpoint+"/login", url.Values{"csrf": {csrf}, "username": {"admin"}, "password": {"synthetic-phase6-password"}})
	if e != nil {
		t.Fatal("fixture login transport failed")
	}
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatal("fixture login rejected")
	}
	csrf = getCSRF("/")
	resp, e = browser.PostForm(endpoint+"/auth/code", url.Values{"csrf": {csrf}, "action": {"approve"}, "code": {code}, "nickname": {nickname}})
	if e != nil {
		t.Fatal("fixture approval transport failed")
	}
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatal("fixture approval rejected")
	}
	c.registered(t)
}
func phase6CurrentFeatures(t *testing.T, c *phase6Client, name string) string {
	t.Helper()
	list := c.call(t, "list_sessions", struct{}{})
	if list.IsError {
		t.Fatal("manual list denied")
	}
	own := phase6Decode[broker.Session](t, c.call(t, "get_session", map[string]string{"name": name}))
	if own.Name != name {
		t.Fatal("manual get returned another session")
	}
	node := phase6Decode[map[string]any](t, c.call(t, "memory_put", map[string]any{"title": "phase6", "body": "phase6-body", "created_by": name}))
	if node["id"] == nil {
		t.Fatal("manual memory create failed")
	}
	if c.call(t, "memory_get", map[string]any{"id": node["id"]}).IsError || c.call(t, "memory_search", map[string]string{"query": "phase6-body"}).IsError {
		t.Fatal("manual memory read/search denied")
	}
	renamed := phase6Decode[broker.Session](t, c.call(t, "rename_session", map[string]string{"name": name + "-renamed"}))
	if renamed.Name != name+"-renamed" {
		t.Fatal("manual rename failed")
	}
	sent := phase6Decode[broker.SendResult](t, c.call(t, "send_message", map[string]string{"from": renamed.Name, "to": renamed.Name, "body": "phase6-manual"}))
	if !sent.Accepted {
		t.Fatal("manual send not accepted")
	}
	phase6Wait(t, "manual native receive", func() bool { data, _ := os.ReadFile(c.queue); return bytes.Contains(data, []byte("phase6-manual")) })
	t.Log("PASS: list/get, memory put/get/search, rename, send/receive through stdio client")
	return renamed.Name
}

func TestPhase6NewClientMatrix(t *testing.T) {
	if os.Getenv("ASB_PHASE6_CLIENT") == "" {
		t.Skip("opt-in binary matrix: set ASB_PHASE6_CLIENT")
	}
	relay := phase6BuildRelay(t)
	if !t.Run("new-client-real-0.14.0-manual", func(t *testing.T) {
		r, parent := phase6OldServer(t)
		probe := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, e := probe.Get(r.server.URL + "/oauth/capabilities?project=test")
		if e != nil {
			t.Fatal("old discovery diagnostic failed")
		}
		response.Body.Close()
		location, _ := url.Parse(response.Header.Get("Location"))
		t.Logf("old discovery HTTP=%d location_path=%s", response.StatusCode, location.Path)
		defer func() {
			t.Logf("wire counts: discovery=%d direct=%d manual_device=%d", r.counts.n("/oauth/capabilities"), r.counts.n("/oauth/session_registration"), r.counts.n("/oauth/device_authorization"))
		}()
		c := phase6Start(t, relay, r.server.URL, parent, "ignored-manual", "", "11111111-1111-4111-8111-111111110604")
		phase6Manual(t, c, r.server.URL, "old-server-worker")
		renamed := phase6CurrentFeatures(t, c, "old-server-worker")
		phase6RestartAndStop(t, c, relay, r, parent, renamed, "11111111-1111-4111-8111-111111110604")
		if r.counts.n("/oauth/session_registration") != 0 {
			t.Fatal("old server received a direct attempt")
		}
	}) {
		return
	}
	if !t.Run("new-client-develop-gate-off-manual", func(t *testing.T) {
		r := phase6Server(t, false)
		parent := r.parent(t, nil, 0)
		c := phase6Start(t, relay, r.server.URL, parent, "ignored-manual", "", "11111111-1111-4111-8111-111111110605")
		phase6Manual(t, c, r.server.URL, "gate-off-worker")
		renamed := phase6CurrentFeatures(t, c, "gate-off-worker")
		phase6RestartAndStop(t, c, relay, r, parent, renamed, "11111111-1111-4111-8111-111111110605")
		if r.counts.n("/oauth/session_registration") != 0 {
			t.Fatal("gate-off server received a direct attempt")
		}
		resp, e := http.Get(r.server.URL + "/oauth/capabilities?project=test")
		if e != nil {
			t.Fatal("gate-off discovery failed")
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if bytes.Contains(body, []byte("direct-registration-v1")) {
			t.Fatal("gate-off discovery advertised direct")
		}
	}) {
		return
	}
	if !t.Run("gate-on-fresh-nickname-and-proof-recovery", func(t *testing.T) {
		r := phase6Server(t, true)
		parent := r.parent(t, phase6AutoScopes(), 8)
		id := "11111111-1111-4111-8111-111111110601"
		c := phase6Start(t, relay, r.server.URL, parent, "named-worker", "", id)
		s := c.registered(t)
		if s["name"] != "named-worker" || s["admissionMode"] != "auto" || r.counts.n("/oauth/device_authorization") != 0 {
			t.Fatal("fresh direct registration contract failed")
		}
		file, state := phase6Saved(t, c)
		registration := state.Registration.RegistrationID
		c.close()
		state.Credential = nil
		data, _ := json.Marshal(state)
		_ = os.WriteFile(file, data, 0600)
		recovered := phase6Start(t, relay, r.server.URL, parent, "named-worker", c.auth, id)
		after := recovered.registered(t)
		if after["registrationId"] != registration {
			t.Fatal("proof recovery changed registration")
		}
		r.counts.mu.Lock()
		proof := r.counts.proof
		r.counts.mu.Unlock()
		if !proof {
			t.Fatal("recovery omitted proof")
		}
		audit, e := r.store.DirectAuditFor("admin", "test")
		if e != nil || len(audit) < 3 {
			t.Fatal("direct audit missing")
		}
		t.Log("PASS: fresh requested nickname, approval=0, existing-registration proof recovery, durable audit")
	}) {
		return
	}
	if !t.Run("restricted-no-receive-send-only-no-read", func(t *testing.T) {
		r := phase6Server(t, true)
		full := r.parent(t, phase6AutoScopes(), 8)
		receiver := phase6Start(t, relay, r.server.URL, full, "receiver", "", "11111111-1111-4111-8111-111111110602")
		receiver.registered(t)
		phase6Wait(t, "recipient readiness", func() bool {
			s := phase6Decode[broker.Session](t, receiver.call(t, "get_session", map[string]string{"name": "receiver"}))
			return s.AutomaticDeliveryReady
		})
		parent := r.parent(t, []string{access.SessionRegister, access.MessageSend, access.SessionRegisterAuto}, 8)
		sender := phase6Start(t, relay, r.server.URL, parent, "sender", "", "11111111-1111-4111-8111-111111110603")
		sender.registered(t)
		for _, tool := range []string{"list_sessions", "get_session"} {
			args := map[string]string{}
			if tool == "get_session" {
				args["name"] = "sender"
			}
			result := sender.call(t, tool, args)
			if !result.IsError || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "insufficient_scope" {
				t.Fatal("no-read result not preserved")
			}
		}
		sent := phase6Decode[broker.SendResult](t, sender.call(t, "send_message", map[string]string{"from": "sender", "to": "receiver", "body": "phase6-send-only"}))
		if !sent.Accepted {
			t.Fatal("send-only not accepted")
		}
		phase6Wait(t, "native queue delivery", func() bool {
			data, _ := os.ReadFile(receiver.queue)
			return bytes.Contains(data, []byte("phase6-send-only"))
		})
		_, saved := phase6Saved(t, sender)
		credential := saved.Credential.AccessToken
		phase6Wait(t, "two receive-free renewals", func() bool { return r.counts.n("renew_lease\x00Bearer "+credential) >= 2 })
		time.Sleep(time.Second)
		if r.counts.n("wait_for_message\x00Bearer "+credential) != 0 {
			t.Fatal("restricted snapshot started receive")
		}
		if status := sender.status(t); status["registered"] != true {
			t.Fatal("receive-free renewal lost registration")
		}
		pending := phase6Decode[broker.Session](t, receiver.call(t, "get_session", map[string]string{"name": "sender"}))
		if pending.AutomaticDeliveryReady {
			t.Fatal("receive-free sender advertised ingress readiness")
		}
		t.Logf("PASS: bounded receive-free renewals=%d receive=0", r.counts.n("renew_lease\x00Bearer "+credential))
		t.Log("PASS: restricted snapshot, send-only delivered, no-read list/get fixed errors")
	}) {
		return
	}
	if !t.Run("cap-boundary", func(t *testing.T) {
		r := phase6Server(t, true)
		parent := r.parent(t, phase6AutoScopes(), 1)
		first := phase6Start(t, relay, r.server.URL, parent, "cap-first", "", "11111111-1111-4111-8111-111111110610")
		first.registered(t)
		second := phase6Start(t, relay, r.server.URL, parent, "cap-second", "", "11111111-1111-4111-8111-111111110611")
		phase6Wait(t, "cap refusal", func() bool { return second.status(t)["status"] == "failed" })
		r.counts.mu.Lock()
		codes := append([]int(nil), r.counts.codes...)
		r.counts.mu.Unlock()
		if len(codes) != 2 || codes[0] != 200 || codes[1] != 409 {
			t.Fatal("cap boundary did not refuse exactly one excess identity")
		}
		project := r.store.Projects()[0]
		if len(project.AutoBindings) != 1 {
			t.Fatal("cap rejection allocated another identity")
		}
		if r.counts.n("/oauth/device_authorization") != 0 {
			t.Fatal("cap error started manual approval")
		}
		t.Log("PASS: cap=1 confirmed identity, excess HTTP409, allocation unchanged")
	}) {
		return
	}
	if !t.Run("rate-limit-retry-after", func(t *testing.T) {
		r := phase6Server(t, true)
		parent := r.parent(t, phase6AutoScopes(), 8)
		for i := 0; i < 3; i++ {
			c := phase6Start(t, relay, r.server.URL, parent, fmt.Sprintf("rate-%d", i), "", fmt.Sprintf("11111111-1111-4111-8111-%012d", 620+i))
			if i == 2 {
				// The third exchange must wait Retry-After=10s. Observe the causal
				// HTTP completion before issuing tools through auto-attach middleware;
				// the ordinary per-tool 8s watchdog is shorter than this valid delay.
				phase6Wait(t, "rate-limited exchange completion", func() bool {
					r.counts.mu.Lock()
					defer r.counts.mu.Unlock()
					return len(r.counts.codes) == 4 && r.counts.codes[3] == 200
				})
			}
			c.registered(t)
		}
		r.counts.mu.Lock()
		codes := append([]int(nil), r.counts.codes...)
		retry := append([]string(nil), r.counts.retry...)
		times := append([]time.Time(nil), r.counts.times...)
		r.counts.mu.Unlock()
		if len(codes) != 4 || codes[0] != 200 || codes[1] != 200 || codes[2] != 429 || codes[3] != 200 {
			t.Fatal("parent burst did not rate-limit then recover")
		}
		seconds, e := strconv.Atoi(retry[2])
		if e != nil || seconds <= 0 || times[3].Sub(times[2]) < time.Duration(seconds)*time.Second {
			t.Fatal("client retried before Retry-After")
		}
		t.Logf("PASS: HTTP429 Retry-After=%ds respected before successful retry", seconds)
	}) {
		return
	}
	if !t.Run("parent-revoke-and-expiry-cascade", func(t *testing.T) {
		for _, expired := range []bool{false, true} {
			if !t.Run(strconv.FormatBool(expired), func(t *testing.T) {
				r := phase6Server(t, true)
				parent := r.parent(t, phase6AutoScopes(), 8)
				c := phase6Start(t, relay, r.server.URL, parent, "cascade", "", fmt.Sprintf("11111111-1111-4111-8111-%012d", 630+btoi(expired)))
				c.registered(t)
				_, state := phase6Saved(t, c)
				child := state.Credential.AccessToken
				grantID := state.Credential.SessionGrantID
				phase6Wait(t, "active receiver", func() bool { return r.counts.n("wait_for_message\x00Bearer "+child) > 0 })
				parentID := ""
				for _, token := range r.store.Projects()[0].Tokens {
					if token.ID == grantID {
						parentID = token.ParentID
					}
				}
				if parentID == "" {
					t.Fatal("server parent binding missing")
				}
				if expired {
					r.offset.Store(int64(31 * 24 * time.Hour))
					if r.store.SweepExpired() != nil {
						t.Fatal("synthetic expiry sweep failed")
					}
				} else if r.store.RevokeToken("test", parentID) != nil {
					t.Fatal("synthetic parent revoke failed")
				}
				phase6Wait(t, "client revoke cascade", func() bool {
					status := c.status(t)
					return status["status"] == "rejected" || status["status"] == "expired"
				})
				req, _ := http.NewRequest("POST", r.server.URL+"/mcp?project=test", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
				req.Header.Set("Authorization", "Bearer "+child)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				response, e := http.DefaultClient.Do(req)
				if e != nil {
					t.Fatal("cascade observer failed")
				}
				response.Body.Close()
				if response.StatusCode != 401 {
					t.Fatal("child still authorized after parent inactivity")
				}
				before := r.counts.n("wait_for_message\x00Bearer " + child)
				time.Sleep(time.Second)
				if r.counts.n("wait_for_message\x00Bearer "+child) != before {
					t.Fatal("inactive parent caused receiver retry storm")
				}
				if len(r.store.Projects()[0].AutoBindings) != 1 {
					t.Fatal("parent inactivity erased retained allocation")
				}
				audit, e := r.store.DirectAuditFor("admin", "test")
				found := false
				for _, event := range audit {
					found = found || (event.ParentID == parentID && event.Reason == "parent-inactive")
				}
				if e != nil || !found {
					t.Fatal("parent cascade audit missing")
				}
				t.Log("PASS: parent inactivity revokes child/receiver, retained allocation and audit remain")
			}) {
				return
			}
		}
	}) {
		return
	}
	if !t.Run("auto-browser-detach-revoke", func(t *testing.T) {
		r := phase6Server(t, true)
		parent := r.parent(t, phase6AutoScopes(), 1)
		c := phase6Start(t, relay, r.server.URL, parent, "browser-worker", "", "11111111-1111-4111-8111-111111110640")
		c.registered(t)
		_, state := phase6Saved(t, c)
		phase6BrowserRevoke(t, r.server.URL, state.Registration.RegistrationID)
		phase6Wait(t, "browser detach credential revoke", func() bool { return c.status(t)["status"] == "rejected" })
		if len(r.store.Projects()[0].AutoBindings) != 0 {
			t.Fatal("browser detach did not release auto allocation")
		}
		replacement := phase6Start(t, relay, r.server.URL, parent, "replacement-worker", "", "11111111-1111-4111-8111-111111110641")
		replacement.registered(t)
		t.Log("PASS: real browser confirmation/CSRF detach revoked child and released cap slot")
	}) {
		return
	}

}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

const phase6BrowserScript = `const {chromium}=require('playwright');const fs=require('node:fs');
(async()=>{const c=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));const browser=await chromium.launch({executablePath:process.env.ASB_PHASE6_BROWSER_EXECUTABLE});try{const page=await browser.newPage();await page.goto(c.url+'/login');await page.locator('input[name="username"]').fill('admin');await page.locator('input[name="password"]').fill('synthetic-phase6-password');await Promise.all([page.waitForURL(u=>u.pathname!='/login'),page.locator('button[type="submit"],button.primary').first().click()]);await page.goto(c.url+'/projects/sessions?project=test');await page.getByRole('button',{name:'Disconnect and remove browser-worker',exact:true}).click();const responsePromise=page.waitForResponse(r=>new URL(r.url()).pathname==='/sessions/detach'&&r.request().method()==='POST');await page.locator('[data-confirm-submit]').click();const response=await responsePromise;const form=new URLSearchParams(response.request().postData());if(response.status()!==303||form.get('id')!==c.id||!form.get('csrf'))throw Error('detach contract');}finally{await browser.close()}})().catch(()=>{console.error('phase6 browser revoke failed');process.exit(1)});`

func phase6BrowserRevoke(t *testing.T, endpoint, id string) {
	t.Helper()
	if os.Getenv("ASB_PHASE6_BROWSER_EXECUTABLE") == "" {
		t.Fatal("browser matrix requires ASB_PHASE6_BROWSER_EXECUTABLE and Playwright on NODE_PATH")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "browser.cjs")
	config := filepath.Join(dir, "fixture.json")
	_ = os.WriteFile(script, []byte(phase6BrowserScript), 0600)
	data, _ := json.Marshal(map[string]string{"url": endpoint, "id": id})
	_ = os.WriteFile(config, data, 0600)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", script, config)
	if cmd.Run() != nil {
		t.Fatal("real browser detach flow failed (details withheld)")
	}
}

func phase6RestartAndStop(t *testing.T, c *phase6Client, relay string, r *phase6Runtime, parent, name, id string) {
	t.Helper()
	_, saved := phase6Saved(t, c)
	registration := saved.Registration.RegistrationID
	before := r.counts.n("resume_session")
	c.close()
	next := phase6Start(t, relay, r.server.URL, parent, "ignored-on-resume", c.auth, id)
	status := next.registered(t)
	if status["registrationId"] != registration || status["name"] != name || r.counts.n("resume_session") <= before {
		t.Fatal("saved credential restart did not resume same registration/name")
	}
	_, saved = phase6Saved(t, next)
	phase6Wait(t, "manual credential renewal", func() bool { return r.counts.n("renew_lease\x00Bearer "+saved.Credential.AccessToken) > 0 })
	stopped := phase6Decode[map[string]any](t, next.call(t, "stop_session", map[string]string{"name": name}))
	if stopped["ok"] != true {
		t.Fatal("manual stop did not detach")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := next.session.ListTools(ctx, nil); err != nil {
		t.Fatal("central stop terminated local stdio harness")
	}
	t.Log("PASS: credential restart/resume/renew, stop detaches without killing local harness")
}
