package serverruntime

// Opt-in integration harness; no helper or release binary ships in artifacts.
// ASB_OLD_CLIENT_ASSETS contains public release-assets.json and raw clients.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

const matrixSID = "11111111-1111-4111-8111-111111111111"
const matrixPeerSID = "22222222-2222-4222-8222-222222222222"

type matrixChildConfig struct{ Binary, Endpoint, Token, AuthDir string }

// A named executable gives the real client's existing parent checks a live,
// controlled Codex process. It never invokes a model or a real Codex session.
func TestOldClientParentHelper(t *testing.T) {
	path := os.Getenv("ASB_MATRIX_CHILD_CONFIG")
	if path == "" {
		t.Skip("parent helper only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		os.Exit(70)
	}
	var c matrixChildConfig
	if json.Unmarshal(raw, &c) != nil {
		os.Exit(70)
	}
	cmd := exec.Command(c.Binary, "--broker-url", c.Endpoint, "--token", c.Token, "--auth-dir", c.AuthDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, io.Discard
	if err := cmd.Run(); err != nil {
		os.Exit(71)
	}
	os.Exit(0) // Do not write Go test's PASS footer into the MCP stream.
}

type matrixClient struct{ session *mcp.ClientSession }

func (c *matrixClient) close() {
	if c != nil && c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
}
func (c *matrixClient) tool(ctx context.Context, name string, args any, out any) bool {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	r, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || r == nil || r.IsError {
		return false
	}
	if out == nil {
		return true
	}
	var raw []byte
	if r.StructuredContent != nil {
		raw, _ = json.Marshal(r.StructuredContent)
	} else if len(r.Content) == 1 {
		if text, ok := r.Content[0].(*mcp.TextContent); ok {
			raw = []byte(text.Text)
		}
	}
	return json.Unmarshal(raw, out) == nil
}
func matrixWait(t *testing.T, step string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("matrix step %s failed (raw errors/results suppressed)", step)
}
func matrixRequire(t *testing.T, ok bool, step string) {
	t.Helper()
	if !ok {
		t.Fatalf("matrix step %s failed (raw errors/results suppressed)", step)
	}
}

func matrixBinary(t *testing.T, root, version string) string {
	t.Helper()
	name := "asb-client-" + version + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	raw, err := os.ReadFile(filepath.Join(root, version, "release-assets.json"))
	matrixRequire(t, err == nil, "release metadata")
	var release struct {
		Assets []struct{ Name, Digest string }
	}
	matrixRequire(t, json.Unmarshal(raw, &release) == nil, "release metadata decode")
	path := filepath.Join(root, version, name)
	data, err := os.ReadFile(path)
	matrixRequire(t, err == nil, "release binary")
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	matched := false
	for _, asset := range release.Assets {
		if asset.Name == name {
			matched = asset.Digest == digest
		}
	}
	matrixRequire(t, matched, "release SHA-256")
	matrixRequire(t, os.Chmod(path, 0o700) == nil, "binary permission")
	result, err := exec.Command(path, "--version").Output()
	matrixRequire(t, err == nil && strings.TrimSpace(string(result)) == "asb-client "+version, "released binary version")
	t.Logf("ASSET %s %s %s PASS", version, name, digest)
	return path
}

func matrixStart(t *testing.T, binary, helper, endpoint, token, authDir, home, cwd string, identity ...string) *matrixClient {
	t.Helper()
	sid := matrixSID
	if len(identity) > 0 {
		sid = identity[0]
	}
	config := matrixChildConfig{binary, endpoint, token, authDir}
	raw, _ := json.Marshal(config)
	path := filepath.Join(t.TempDir(), "child.json")
	matrixRequire(t, os.WriteFile(path, raw, 0o600) == nil, "private child config")
	cmd := exec.Command(helper, "-test.run=^TestOldClientParentHelper$")
	// Do not inherit real harness endpoints, credentials, rollout locations or state.
	cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "CODEX_HOME=" + filepath.Join(home, ".codex"), "CODEX_THREAD_ID=" + sid, "ASB_MATRIX_CHILD_CONFIG=" + path}
	cmd.Dir = cwd
	cmd.Stderr = io.Discard
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-compatibility-harness", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
	matrixRequire(t, err == nil, "stdio initialize")
	c := &matrixClient{session}
	t.Cleanup(c.close)
	return c
}

// Produce a v3 synthetic fixture using the same metadata removal as the
// existing migration tests. Issued raw credentials never leave private memory.
func matrixSeed(t *testing.T, dir, clientID string) (parent, session, legacy, peer string, before map[string]string) {
	t.Helper()
	control := filepath.Join(dir, "control")
	s, err := controlpanel.OpenSQLite(control)
	matrixRequire(t, err == nil, "seed open")
	matrixRequire(t, s.Setup("synthetic-matrix-password") == nil && s.CreateProject("test") == nil, "seed setup")
	matrixRequire(t, s.SetInstanceHTTPPolicy("admin", true) == nil, "isolated HTTP opt-in")
	parent, err = s.IssueProjectTokenFor("admin", "test", "old parent")
	matrixRequire(t, err == nil, "seed parent")
	legacy, err = s.IssueTokenFor("admin", "test", "legacy", "admin")
	matrixRequire(t, err == nil, "seed legacy")
	issue := func(sid, instance, name string) string {
		code, e := s.BeginDeviceAuthorization("test", parent, controlpanel.DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: sid, InstanceID: instance, RequestID: "seed-" + name})
		matrixRequire(t, e == nil, "seed device")
		_, e = s.ApproveDeviceAuthorization("admin", code.UserCode, name, false)
		matrixRequire(t, e == nil, "seed approval")
		grant, e := s.ExchangeDeviceToken("test", parent, code.DeviceCode, "asb-client-v1", false)
		matrixRequire(t, e == nil, "seed exchange")
		return grant.AccessToken
	}
	session = issue(matrixSID, clientID, "session")
	peer = issue(matrixPeerSID, "matrix-peer-client", "peer")
	matrixRequire(t, s.Close() == nil, "seed close")
	db, err := sqlitestore.Open(control, "control.db")
	matrixRequire(t, err == nil, "fixture DB open")
	raw, err := db.Get("state")
	matrixRequire(t, err == nil, "fixture read")
	var state map[string]any
	matrixRequire(t, json.Unmarshal(raw, &state) == nil, "fixture decode")
	state["version"] = 3
	before = map[string]string{}
	for _, p := range state["projects"].(map[string]any) {
		project := p.(map[string]any)
		delete(project, "projectGenerationId") // v3 predates project generations.
		for _, v := range project["tokens"].([]any) {
			token := v.(map[string]any)
			before[token["id"].(string)] = token["tokenHash"].(string)
			for _, key := range []string{"scopeVersion", "scopes", "admissionMode", "issuedBy", "maxAutoBindings"} {
				delete(token, key)
			}
		}
		if devices, ok := project["devices"].([]any); ok {
			for _, v := range devices {
				d := v.(map[string]any)
				delete(d, "scopeVersion")
				delete(d, "scopes")
			}
		}
	}
	raw, _ = json.Marshal(state)
	matrixRequire(t, db.Put("state", raw) == nil && db.Close() == nil, "write old v3 fixture")
	return
}

func matrixVerifyUpgrade(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(dir, "control"), "control.db")
	matrixRequire(t, err == nil, "upgrade DB open")
	defer db.Close()
	raw, err := db.Get("state")
	matrixRequire(t, err == nil, "upgrade DB read")
	var state struct {
		Version  int
		Projects map[string]struct {
			Tokens []struct {
				ID, TokenHash string
				ScopeVersion  int
				Scopes        []string
			}
		}
	}
	matrixRequire(t, json.Unmarshal(raw, &state) == nil && state.Version == 4, "upgrade payload v4")
	found := 0
	for _, p := range state.Projects {
		for _, token := range p.Tokens {
			if hash, ok := before[token.ID]; ok {
				matrixRequire(t, hash == token.TokenHash && token.ScopeVersion == 1 && slices.Equal(token.Scopes, access.LegacyV1Scopes()), "upgrade retains hash and LEGACY_V1")
				found++
			}
		}
	}
	matrixRequire(t, found == len(before), "upgrade keeps all old credentials")
}

func TestPublicOldClientsScopeMatrix(t *testing.T) {
	assets := os.Getenv("ASB_OLD_CLIENT_ASSETS")
	if assets == "" {
		t.Skip("opt-in: scripts/tests/token_scope_old_clients.sh downloads SHA-verified real clients")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("named Codex helper supports Unix hosts")
	}
	self, err := os.Executable()
	matrixRequire(t, err == nil, "test helper executable")
	helper := filepath.Join(t.TempDir(), "codex")
	data, err := os.ReadFile(self)
	matrixRequire(t, err == nil && os.WriteFile(helper, data, 0o700) == nil, "named parent helper")
	for _, version := range []string{"0.12.0", "0.13.0", "0.14.0"} {
		binary := matrixBinary(t, assets, version)
		for _, mode := range []string{"project", "session", "legacy"} {
			if !t.Run(version+"/"+mode, func(t *testing.T) { matrixCase(t, binary, helper, version, mode) }) {
				t.FailNow()
			}
		}
	}
}

func matrixCase(t *testing.T, binary, helper, version, mode string) {
	dir, home, authDir, cwd := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	rolloutDir := filepath.Join(home, ".codex", "sessions", "2026", "10", "14")
	matrixRequire(t, os.MkdirAll(rolloutDir, 0o700) == nil, "synthetic rollout directory")
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": matrixSID, "cwd": cwd}})
	matrixRequire(t, os.WriteFile(filepath.Join(rolloutDir, "rollout-synthetic-"+matrixSID+".jsonl"), append(meta, '\n'), 0o600) == nil, "synthetic identity metadata")
	localStore, err := clientstate.Open(authDir)
	matrixRequire(t, err == nil, "local state open")
	clientID, err := localStore.ClientID()
	matrixRequire(t, err == nil, "local identity")
	parent, session, legacy, peer, before := matrixSeed(t, dir, clientID)
	var handler atomic.Value
	placeholder := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "starting", http.StatusServiceUnavailable) })
	handler.Store(http.Handler(placeholder))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.Load().(http.Handler).ServeHTTP(w, r) }))
	defer server.Close()
	endpoint := server.URL + "/mcp?project=test"
	var closeRuntime func() error
	start := func() {
		h, closeHandler, e := newManagedHandler(dir, nil)
		matrixRequire(t, e == nil, "develop runtime start")
		handler.Store(h)
		closeRuntime = closeHandler
	}
	start()
	defer func() {
		matrixRequire(t, closeRuntime() == nil, "develop runtime shutdown")
		matrixVerifyUpgrade(t, dir, before)
	}()
	browser := &deviceRuntime{t: t, h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.Load().(http.Handler).ServeHTTP(w, r) }), project: "test", password: "synthetic-matrix-password"}
	browser.login(browser.password)
	// Gate-off checks exercise real browser RBAC/CSRF and real OAuth routes.
	for _, scopes := range [][]string{{"session:register"}, {"session:register", "session:register:auto"}} {
		form := url.Values{"project": {"test"}, "label": {"unavailable"}, "scope_selection": {"custom"}, "scope": scopes}
		if len(scopes) > 1 {
			form.Set("auto_cap", "8")
			form.Set("auto_days", "30")
		}
		w := browser.browser("POST", "/tokens/project", form)
		matrixRequire(t, w.Code == 400 && strings.Contains(w.Body.String(), "호환성 검증 완료"), "gate-off restricted/auto issuance")
	}
	status, _ := browser.rest("/oauth/session_registration?project=test", parent, url.Values{"client_id": {"asb-client-v1"}})
	matrixRequire(t, status == 403, "gate-off direct endpoint")
	token, name := parent, "approved"
	if mode == "legacy" {
		token, name = legacy, "legacy"
	}
	if mode == "session" {
		token, name = session, "session"
		scope := strings.ToLower(server.URL) + "/mcp?project=test"
		key := clientstate.SessionKey(scope, "codex", matrixSID, clientID)
		matrixRequire(t, localStore.SaveSession(key, clientstate.SessionState{Scope: scope, Project: "test", Harness: "codex", SessionID: matrixSID, ClientID: clientID, Registration: clientstate.Registration{Secret: clientstate.NewSecret()}, Credential: &clientstate.SessionCredential{AccessToken: session, TokenType: "Bearer", Nickname: "session", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}) == nil, "existing session local state")
	}
	c := matrixStart(t, binary, helper, endpoint, token, authDir, home, cwd)
	defer func() { c.close() }()
	if mode == "project" {
		var auth struct{ Status, UserCode string }
		matrixWait(t, "device pending", func() bool {
			return c.tool(t.Context(), "auth_status", map[string]any{}, &auth) && auth.Status == "pending" && auth.UserCode != ""
		})
		w := browser.browser("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {auth.UserCode}, "nickname": {name}})
		matrixRequire(t, w.Code == 303, "device approval")
		t.Logf("MATRIX %s %s device-approval PASS", version, mode)
	}
	var own broker.Session
	matrixWait(t, "register-ready", func() bool {
		return c.tool(t.Context(), "get_session", map[string]string{"name": name}, &own) && own.AutomaticDeliveryReady
	})
	registrationID := own.RegistrationID
	matrixRequire(t, registrationID != "", "registration ID")
	t.Logf("MATRIX %s %s register/get-session PASS", version, mode)
	var listed []broker.Session
	matrixRequire(t, c.tool(t.Context(), "list_sessions", map[string]any{}, &listed) && len(listed) > 0, "list sessions")
	// Current HTTP SDK peer supplies a real activity record. Old binaries must
	// still decode get/list even when another registration has activity.
	peerClient := matrixHTTPPeer(t, server.URL, peer)
	var peerReg broker.RegisterResult
	matrixRequire(t, peerClient.tool(t.Context(), "register_session", broker.RegisterRequest{Harness: "codex", SessionID: matrixPeerSID, ClientID: "matrix-peer-client", PID: os.Getpid(), CWD: cwd, Name: "peer", ChannelID: "peer-lease", Secret: clientstate.NewSecret(), RequestID: "peer-register"}, &peerReg) && peerReg.Code == "", "peer registration")
	sample, _ := json.Marshal(protocol.Activity{State: protocol.ActivityWorking, Source: "codex-rollout", Sequence: 1, ObservedAt: time.Now().UTC()})
	var renewed broker.RegisterResult
	matrixRequire(t, peerClient.tool(t.Context(), "renew_lease", broker.RenewRequest{RegistrationID: peerReg.Session.RegistrationID, ChannelID: "peer-lease", Activity: sample}, &renewed) && renewed.Code == "" && renewed.Session.Activity != nil, "peer activity evidence")
	var other broker.Session
	matrixRequire(t, c.tool(t.Context(), "get_session", map[string]string{"name": "peer"}, &other) && c.tool(t.Context(), "list_sessions", map[string]any{}, &listed), "old activity output schemas")
	t.Logf("MATRIX %s %s list/get-with-activity PASS", version, mode)
	matrixRequire(t, c.tool(t.Context(), "ingress_hook", map[string]string{"session_id": matrixSID, "hook_event_name": "PreToolUse"}, nil), "ingress hook start")
	var sent broker.SendResult
	matrixRequire(t, c.tool(t.Context(), "send_message", broker.Message{From: name, To: name, Body: "synthetic-phase6-delivery"}, &sent) && sent.Accepted, "send")
	var hook map[string]any
	matrixWait(t, "receive via real ingress hook", func() bool {
		if !c.tool(t.Context(), "ingress_hook", map[string]string{"session_id": matrixSID, "hook_event_name": "PostToolUse"}, &hook) {
			return false
		}
		raw, _ := json.Marshal(hook)
		return strings.Contains(string(raw), "synthetic-phase6-delivery")
	})
	t.Logf("MATRIX %s %s send/receive PASS", version, mode)
	var node memory.Node
	matrixRequire(t, c.tool(t.Context(), "memory_put", memory.PutInput{Title: "phase6", Body: "synthetic-phase6-memory", CreatedBy: name}, &node) && node.ID != "", "memory put")
	var got memory.GetResult
	var found memory.SearchResult
	matrixRequire(t, c.tool(t.Context(), "memory_get", map[string]string{"id": node.ID}, &got) && got.Node.Body == "synthetic-phase6-memory" && c.tool(t.Context(), "memory_search", memory.SearchInput{Query: "synthetic-phase6-memory"}, &found) && len(found.Nodes) == 1, "memory get/search")
	t.Logf("MATRIX %s %s memory-put/get/search PASS", version, mode)
	name = "renamed-" + mode
	matrixRequire(t, c.tool(t.Context(), "rename_session", map[string]string{"name": name}, &own) && own.Name == name && own.RegistrationID == registrationID, "rename")
	c.close()
	c = matrixStart(t, binary, helper, endpoint, token, authDir, home, cwd)
	matrixWait(t, "client restart resume", func() bool {
		return c.tool(t.Context(), "get_session", map[string]string{"name": name}, &own) && own.RegistrationID == registrationID && own.AutomaticDeliveryReady
	})
	generation := own.Generation
	t.Logf("MATRIX %s %s rename/client-restart-resume PASS", version, mode)
	peerClient.close()
	matrixRequire(t, closeRuntime() == nil, "runtime restart stop")
	start()
	observer := matrixHTTPPeer(t, server.URL, peer)
	defer observer.close()
	matrixWait(t, "server restart automatic resume", func() bool {
		return observer.tool(t.Context(), "get_session", map[string]string{"name": name}, &own) && own.RegistrationID == registrationID && own.Name == name && own.Generation > generation && own.AutomaticDeliveryReady
	})
	matrixRequire(t, c.tool(t.Context(), "get_session", map[string]string{"name": name}, &own), "get after restart")
	t.Logf("MATRIX %s %s server-restart/same-registration-resume PASS", version, mode)
	var stopped struct {
		OK bool `json:"ok"`
	}
	matrixRequire(t, c.tool(t.Context(), "stop_session", map[string]string{"name": name}, &stopped) && stopped.OK, "stop")
	t.Logf("MATRIX %s %s stop/gate-off/migration-hashes PASS", version, mode)
}

func matrixHTTPPeer(t *testing.T, origin, token string) *matrixClient {
	t.Helper()
	client := &http.Client{Transport: matrixBearerTransport{token: token}, Timeout: 10 * time.Second}
	s, err := mcp.NewClient(&mcp.Implementation{Name: "phase6-server-observer", Version: "0.17.3"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: origin + "/mcp?project=test", HTTPClient: client}, nil)
	matrixRequire(t, err == nil, "HTTP observer initialize")
	c := &matrixClient{s}
	t.Cleanup(c.close)
	return c
}

type matrixBearerTransport struct{ token string }

func (x matrixBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer "+x.token)
	return http.DefaultTransport.RoundTrip(copy)
}
