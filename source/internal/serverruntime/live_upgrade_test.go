package serverruntime

// Verification only: every server and stdio child is owned by this opt-in test.
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
	"fmt"
	"html"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

// All content and raw errors stay in memory. Failure logs use fixed step names.
func upgradeBinary(t *testing.T, root, version, component string) string {
	t.Helper()
	if component == "client" && version != "0.15.0" && version != "0.16.0" {
		return matrixBinary(t, root, version)
	}
	name := "asb-" + component + "-" + version + "-darwin-arm64"
	path := filepath.Join(root, version, name)
	data, err := os.ReadFile(path)
	matrixRequire(t, err == nil, "upgrade asset read")
	if component == "server" {
		raw, e := os.ReadFile(filepath.Join(root, version, "release-assets.json"))
		matrixRequire(t, e == nil, "server release metadata")
		var release struct {
			Assets []struct{ Name, Digest string }
		}
		matrixRequire(t, json.Unmarshal(raw, &release) == nil, "server metadata decode")
		matched := false
		for _, a := range release.Assets {
			if a.Name == name {
				matched = a.Digest == upgradeSHA(data)
			}
		}
		matrixRequire(t, matched, "server Release SHA256")
	} else {
		raw, e := os.ReadFile(filepath.Join(root, version, "npm.tgz"))
		matrixRequire(t, e == nil, "npm client package")
		meta, e := os.ReadFile(filepath.Join(root, version, "registry-dist.json"))
		matrixRequire(t, e == nil, "npm integrity metadata")
		var dist struct{ Integrity string }
		matrixRequire(t, json.Unmarshal(meta, &dist) == nil, "npm metadata decode")
		sum := sha512.Sum512(raw)
		matrixRequire(t, dist.Integrity == "sha512-"+base64.StdEncoding.EncodeToString(sum[:]), "npm SHA512")
		gz, e := gzip.NewReader(bytes.NewReader(raw))
		matrixRequire(t, e == nil, "npm archive open")
		defer gz.Close()
		tr := tar.NewReader(gz)
		matched := false
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			matrixRequire(t, e == nil, "npm member decode")
			if h.Name == "package/bin/darwin-arm64/asb-client" {
				b, e := io.ReadAll(io.LimitReader(tr, 64<<20))
				matrixRequire(t, e == nil, "npm member read")
				matched = bytes.Equal(b, data)
			}
		}
		matrixRequire(t, matched, "npm embedded client bytes")
	}
	matrixRequire(t, os.Chmod(path, 0700) == nil, "asset executable mode")
	out, err := exec.Command(path, "--version").Output()
	matrixRequire(t, err == nil && strings.TrimSpace(string(out)) == "asb-"+component+" "+version, "asset version")
	t.Logf("UPGRADE_ASSET %s %s PASS", name, upgradeSHA(data))
	return path
}

// SHA helper is separate from the expected public asset metadata.
func upgradeSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type upgradeServer struct {
	t            *testing.T
	address, dir string
	cmd          *exec.Cmd
	done         chan struct{}
}

func (s *upgradeServer) start(binary string) time.Time {
	s.cmd = exec.Command(binary, "--listen", s.address, "--memory-dir", s.dir)
	s.cmd.Stdout, s.cmd.Stderr = io.Discard, io.Discard
	matrixRequire(s.t, s.cmd.Start() == nil, "public server launch")
	s.done = make(chan struct{})
	cmd, done := s.cmd, s.done
	go func() { _ = cmd.Wait(); close(done) }()
	matrixWait(s.t, "public server ready", func() bool {
		r, e := http.Get("http://" + s.address + "/healthz")
		if e != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
	return time.Now()
}
func (s *upgradeServer) stop() {
	if s.cmd == nil {
		return
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case <-s.done:
	case <-time.After(8 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
		s.t.Error("owned server shutdown exceeded watchdog")
	}
	s.cmd = nil
}
func upgradeVersion(t *testing.T, dir string) int {
	db, e := sqlitestore.Open(filepath.Join(dir, "control"), "control.db")
	matrixRequire(t, e == nil, "control version open")
	defer db.Close()
	raw, e := db.Get("state")
	matrixRequire(t, e == nil, "control version read")
	var state struct{ Version int }
	matrixRequire(t, json.Unmarshal(raw, &state) == nil, "control version decode")
	return state.Version
}

type upgradeBrowser struct {
	observe func(string, string, int, []byte)
	t       *testing.T
	origin  string
	client  *http.Client
}

func (b *upgradeBrowser) get(path string) string {
	r, e := b.client.Get(b.origin + path)
	if e != nil && b.observe != nil {
		b.observe("GET", path, 0, nil)
	}
	matrixRequire(b.t, e == nil, "fixture browser GET")
	defer r.Body.Close()
	data, e := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if b.observe != nil {
		b.observe("GET", path, r.StatusCode, data)
	}
	matrixRequire(b.t, e == nil && r.StatusCode == 200, "fixture page")
	return string(data)
}
func (b *upgradeBrowser) post(path, page string, values url.Values) string {
	matches := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(b.get(page))
	matrixRequire(b.t, len(matches) == 2, "fixture CSRF")
	values.Set("csrf", html.UnescapeString(matches[1]))
	r, e := b.client.PostForm(b.origin+path, values)
	if e != nil && b.observe != nil {
		b.observe("POST", path, 0, nil)
	}
	matrixRequire(b.t, e == nil, "fixture form POST")
	defer r.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if b.observe != nil {
		b.observe("POST", path, r.StatusCode, raw)
	}
	matrixRequire(b.t, r.StatusCode == 303, "fixture form accepted")
	return r.Header.Get("Location")
}
func (b *upgradeBrowser) login() {
	b.post("/login", "/login", url.Values{"username": {"admin"}, "password": {"synthetic-upgrade-password"}})
}
func (b *upgradeBrowser) approve(code, name string) {
	b.post("/auth/code", "/auth/code", url.Values{"action": {"approve"}, "code": {code}, "nickname": {name}})
}
func (b *upgradeBrowser) grant(parent, sid, instance, name string) string {
	return b.grantHarness(parent, sid, instance, name, "codex")
}
func (b *upgradeBrowser) grantHarness(parent, sid, instance, name, harness string) string {
	rest := func(path string, v url.Values, out any) {
		req, _ := http.NewRequest("POST", b.origin+path+"?project=test", strings.NewReader(v.Encode()))
		req.Header.Set("Authorization", "Bearer "+parent)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r, e := http.DefaultClient.Do(req)
		if e != nil && b.observe != nil {
			b.observe("POST", path, 0, nil)
		}
		matrixRequire(b.t, e == nil, "fixture OAuth transport")
		defer r.Body.Close()
		raw, e := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if b.observe != nil {
			b.observe("POST", path, r.StatusCode, raw)
		}
		matrixRequire(b.t, e == nil && r.StatusCode == 200, "fixture OAuth status")
		matrixRequire(b.t, json.Unmarshal(raw, out) == nil, "fixture OAuth decode")
	}
	var code struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	rest("/oauth/device_authorization", url.Values{"client_id": {"asb-client-v1"}, "harness": {harness}, "session_id": {sid}, "asb_client_instance_id": {instance}, "request_id": {"fixture-" + name}}, &code)
	b.approve(code.UserCode, name)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	rest("/oauth/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {"asb-client-v1"}, "device_code": {code.DeviceCode}}, &token)
	matrixRequire(b.t, token.AccessToken != "", "fixture grant token")
	return token.AccessToken
}

const upgradeLegacySource = `package main
import("encoding/json";"os";"path/filepath";"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel")
func main(){s,e:=controlpanel.OpenSQLite(filepath.Join(os.Args[1],"control"));if e!=nil{os.Exit(1)};defer s.Close();tokens:=[]string{};for _,name:=range []string{"v013-legacy","v014-legacy","v015-legacy"}{raw,e:=s.IssueTokenFor("admin","test",name,"admin");if e!=nil{os.Exit(2)};tokens=append(tokens,raw)};raw,_:=json.Marshal(tokens);if os.WriteFile(os.Args[2],raw,0600)!=nil{os.Exit(3)}}`

func upgradeLegacyBuilder(t *testing.T) string {
	source := os.Getenv("ASB_UPGRADE_OLD_SOURCE")
	matrixRequire(t, source != "", "old source provided")
	cmdDir := filepath.Join(source, "cmd", "upgradeseed")
	matrixRequire(t, os.MkdirAll(cmdDir, 0700) == nil, "old legacy helper directory")
	matrixRequire(t, os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte(upgradeLegacySource), 0600) == nil, "old legacy helper source")
	binary := filepath.Join(t.TempDir(), "legacy-seed")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/upgradeseed")
	cmd.Dir = source
	matrixRequire(t, cmd.Run() == nil, "old legacy helper build")
	return binary
}

type upgradeConnection struct {
	version, mode, sid, name, auth string
	client                         *matrixClient
	cmd                            *exec.Cmd
	before                         broker.Session
	recovered                      float64
	authSupported                  bool
	proof                          upgradeLocalProof
	inbox                          *upgradeInbox
	home                           string
	observations                   int
}

// Selected fixture-only evidence. Credentials and request identifiers never log.
type upgradeLocalProof struct{ credential, deviceRequest, deviceStatus string }

func upgradeProof(t *testing.T, auth string) upgradeLocalProof {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(auth, "sessions", "*", "conn.json"))
	matrixRequire(t, err == nil && len(files) <= 1, "isolated local proof files")
	if len(files) == 0 {
		return upgradeLocalProof{}
	}
	raw, err := os.ReadFile(files[0])
	matrixRequire(t, err == nil, "local proof read")
	var state clientstate.SessionState
	matrixRequire(t, json.Unmarshal(raw, &state) == nil, "local proof decode")
	var proof upgradeLocalProof
	if state.Credential != nil {
		proof.credential = state.Credential.AccessToken
	}
	if state.Device != nil {
		proof.deviceRequest, proof.deviceStatus = state.Device.RequestID, state.Device.Status
		matrixRequire(t, state.Device.Status != clientstate.DevicePending && state.Device.Status != clientstate.DeviceRequested, "no local device requester")
	}
	return proof
}

func upgradeHasAuth(t *testing.T, c *matrixClient) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	for tool, err := range c.session.Tools(ctx, nil) {
		matrixRequire(t, err == nil, "auth tool discovery")
		if tool.Name == "auth_status" {
			return true
		}
	}
	return false
}

func upgradeControlProof(t *testing.T, dir string) (map[string]string, map[string]string) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(dir, "control"), "control.db")
	matrixRequire(t, err == nil, "control proof open")
	defer db.Close()
	raw, err := db.Get("state")
	matrixRequire(t, err == nil, "control proof read")
	var state struct {
		Projects map[string]struct {
			Tokens  []struct{ ID, TokenHash string }
			Devices []struct {
				CodeHash string
				Request  struct{ RequestID string }
			}
		}
	}
	matrixRequire(t, json.Unmarshal(raw, &state) == nil, "control proof decode")
	hashes, requests := map[string]string{}, map[string]string{}
	for _, project := range state.Projects {
		for _, token := range project.Tokens {
			hashes[token.ID] = token.TokenHash
		}
		for _, device := range project.Devices {
			requests[device.Request.RequestID] = device.CodeHash
		}
	}
	matrixRequire(t, len(hashes) > 0 && len(requests) > 0, "nonempty original control evidence")
	return hashes, requests
}

func upgradeStart(t *testing.T, binary, helper, endpoint, token, auth, home, cwd, sid string) (*matrixClient, *exec.Cmd) {
	config, _ := json.Marshal(matrixChildConfig{binary, endpoint, token, auth})
	path := filepath.Join(t.TempDir(), "child.json")
	matrixRequire(t, os.WriteFile(path, config, 0600) == nil, "private stdio config")
	cmd := exec.Command(helper, "-test.run=^TestOldClientParentHelper$")
	cmd.Dir = cwd
	cmd.Stderr = io.Discard
	cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "CODEX_HOME=" + filepath.Join(home, ".codex"), "CODEX_THREAD_ID=" + sid, "ASB_MATRIX_CHILD_CONFIG=" + path}
	session, e := mcp.NewClient(&mcp.Implementation{Name: "synthetic-live-upgrade", Version: "1"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}, nil)
	matrixRequire(t, e == nil, "stdio initialize")
	c := &matrixClient{session}
	t.Cleanup(c.close)
	return c, cmd
}
func upgradeAlive(c *upgradeConnection) bool { return c.cmd.Process.Signal(syscall.Signal(0)) == nil }
func upgradeErrorShape(ctx context.Context, c *matrixClient) string {
	deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	r, e := c.session.CallTool(deadline, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if deadline.Err() != nil {
		return "deadline"
	}
	if e != nil {
		return "rpc-error"
	}
	if r != nil && r.IsError {
		return "tool-error"
	}
	return "success"
}

func TestPublicLiveServerUpgrade(t *testing.T) {
	assets := os.Getenv("ASB_UPGRADE_ASSETS")
	if assets == "" {
		t.Skip("opt-in: scripts/tests/live_upgrade_recovery.sh")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("public darwin-arm64 verification")
	}
	oldServer, newServer := upgradeBinary(t, assets, "0.14.0", "server"), upgradeBinary(t, assets, "0.15.0", "server")
	clients := map[string]string{}
	for _, v := range []string{"0.13.0", "0.14.0", "0.15.0"} {
		clients[v] = upgradeBinary(t, assets, v, "client")
	}
	self, e := os.Executable()
	matrixRequire(t, e == nil, "helper executable")
	data, e := os.ReadFile(self)
	matrixRequire(t, e == nil, "helper read")
	helper := filepath.Join(t.TempDir(), "codex")
	matrixRequire(t, os.WriteFile(helper, data, 0700) == nil, "named synthetic parent")
	legacyBuilder := upgradeLegacyBuilder(t)
	for _, variant := range []string{"quick", "long", "during-call"} {
		if !t.Run(variant, func(t *testing.T) { upgradeVariant(t, oldServer, newServer, legacyBuilder, helper, clients, variant) }) {
			t.FailNow()
		}
	}
}

func upgradeVariant(t *testing.T, oldServer, newServer, legacyBuilder, helper string, binaries map[string]string, variant string, harnesses ...string) {
	harness := "codex"
	if len(harnesses) > 0 {
		harness = harnesses[0]
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	matrixRequire(t, e == nil, "isolated port")
	address := listener.Addr().String()
	listener.Close()
	server := &upgradeServer{t: t, address: address, dir: t.TempDir()}
	t.Cleanup(server.stop)
	server.start(oldServer)
	jar, _ := cookiejar.New(nil)
	browser := &upgradeBrowser{t: t, origin: "http://" + address, client: &http.Client{Jar: jar, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	browser.post("/setup", "/setup", url.Values{"password": {"synthetic-upgrade-password"}, "confirmPassword": {"synthetic-upgrade-password"}})
	browser.login()
	browser.post("/projects/create", "/", url.Values{"name": {"test"}})
	browser.post("/admin/http-policy", "/admin/settings", url.Values{"allowHTTP": {"true"}})
	issued := browser.post("/tokens/project", "/projects/settings?project=test", url.Values{"project": {"test"}, "label": {"upgrade-parent"}})
	tokenPage := browser.get(issued)
	matches := regexp.MustCompile(`<code id="issued-token">([^<]+)</code>`).FindStringSubmatch(tokenPage)
	matrixRequire(t, len(matches) == 2, "one-time fixture parent")
	parent := html.UnescapeString(matches[1])
	matrixRequire(t, upgradeVersion(t, server.dir) == 3, "real old server DB v3")
	server.stop()
	legacyFile := filepath.Join(t.TempDir(), "legacy.json")
	matrixRequire(t, exec.Command(legacyBuilder, server.dir, legacyFile).Run() == nil, "old API legacy issuance")
	raw, _ := os.ReadFile(legacyFile)
	var legacy []string
	matrixRequire(t, json.Unmarshal(raw, &legacy) == nil && len(legacy) == 3, "private legacy fixture")
	matrixRequire(t, upgradeVersion(t, server.dir) == 3, "legacy fixture remains v3")
	server.start(oldServer)
	browser.login()
	connections := []*upgradeConnection{}
	versions := []string{"0.13.0", "0.14.0", "0.15.0"}
	if harness == "claude" {
		versions = []string{"0.14.0", "0.15.0", "0.13.0"}
	}
	legacyByVersion := map[string]string{"0.13.0": legacy[0], "0.14.0": legacy[1], "0.15.0": legacy[2]}
	for vi, version := range versions {
		for mi, mode := range []string{"project", "session", "legacy"} {
			sid := fmt.Sprintf("11111111-1111-4111-8111-%012d", vi*3+mi+1)
			name := "v0" + strings.TrimSuffix(version[2:], ".0") + "-" + mode
			home, cwd, auth := t.TempDir(), t.TempDir(), t.TempDir()
			rollouts := filepath.Join(home, ".codex", "sessions", "2026", "10", "15")
			matrixRequire(t, os.MkdirAll(rollouts, 0700) == nil, "synthetic rollout directory")
			meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": sid, "cwd": cwd}})
			matrixRequire(t, os.WriteFile(filepath.Join(rollouts, "rollout-synthetic-"+sid+".jsonl"), append(meta, '\n'), 0600) == nil, "synthetic rollout")
			store, e := clientstate.Open(auth)
			matrixRequire(t, e == nil, "isolated clientstate")
			clientID, e := store.ClientID()
			matrixRequire(t, e == nil, "client instance")
			token := parent
			if mode == "legacy" {
				token = legacyByVersion[version]
			}
			endpoint := browser.origin + "/mcp?project=test"
			if mode == "session" {
				token = browser.grantHarness(parent, sid, clientID, name, harness)
				scope := strings.ToLower(endpoint)
				key := clientstate.SessionKey(scope, harness, sid, clientID)
				matrixRequire(t, store.SaveSession(key, clientstate.SessionState{Scope: scope, Project: "test", Harness: harness, SessionID: sid, ClientID: clientID, Registration: clientstate.Registration{Secret: clientstate.NewSecret()}, Credential: &clientstate.SessionCredential{AccessToken: token, TokenType: "Bearer", Nickname: name, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}) == nil, "session credential fixture")
			}
			var c *matrixClient
			var cmd *exec.Cmd
			var inbox *upgradeInbox
			if harness == "claude" {
				inbox = newUpgradeInbox(t)
				c, cmd = upgradeClaudeStart(t, binaries[version], helper, endpoint, token, auth, home, cwd, sid, inbox)
			} else {
				c, cmd = upgradeStart(t, binaries[version], helper, endpoint, token, auth, home, cwd, sid)
			}
			conn := &upgradeConnection{version: version, mode: mode, sid: sid, name: name, auth: auth, client: c, cmd: cmd, inbox: inbox, home: home}
			if mode == "project" {
				var status struct{ Status, UserCode string }
				matrixWait(t, "initial device pending", func() bool {
					return c.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Status == "pending" && status.UserCode != ""
				})
				browser.approve(status.UserCode, name)
			}
			matrixWait(t, "old server registration ready", func() bool {
				return c.tool(t.Context(), "get_session", map[string]string{"name": name}, &conn.before) && conn.before.AutomaticDeliveryReady
			})
			matrixRequire(t, conn.before.RegistrationID != "" && upgradeAlive(conn), "old stdio process alive")
			conn.authSupported = upgradeHasAuth(t, c)
			conn.proof = upgradeProof(t, auth)
			if inbox != nil {
				conn.observations = upgradeClaudeObservations(t, home)
				matrixRequire(t, conn.observations >= 2, "Claude parent identity inspected")
			}
			t.Logf("AUTH_CAPABILITY %s %s auth_status=%t", version, mode, conn.authSupported)
			connections = append(connections, conn)
		}
	}
	peerToken := browser.grant(parent, matrixPeerSID, "upgrade-peer-client", "peer")
	observer := matrixHTTPPeer(t, browser.origin, peerToken)
	secret := clientstate.NewSecret()
	var peer broker.RegisterResult
	matrixRequire(t, observer.tool(t.Context(), "register_session", broker.RegisterRequest{Harness: "codex", SessionID: matrixPeerSID, ClientID: "upgrade-peer-client", PID: os.Getpid(), CWD: server.dir, Name: "peer", ChannelID: "peer-old", Secret: secret, RequestID: "peer-first"}, &peer) && peer.Code == "", "queue peer register")
	var sent broker.SendResult
	matrixRequire(t, connections[0].client.tool(t.Context(), "send_message", broker.Message{From: connections[0].name, To: "peer", Body: "queued-before-upgrade"}, &sent) && sent.Accepted, "pre-restart queue send")
	var queued broker.Session
	matrixRequire(t, observer.tool(t.Context(), "get_session", map[string]string{"name": "peer"}, &queued) && queued.PendingMessages == 1, "pre-restart queue present")
	beforeHashes, beforeRequests := upgradeControlProof(t, server.dir)
	observer.close()
	server.stop()
	down := time.Now()
	if variant == "during-call" {
		for _, c := range connections {
			shape := upgradeErrorShape(t.Context(), c.client)
			matrixRequire(t, upgradeAlive(c), "stdio survives downtime call")
			t.Logf("DOWNTIME_CALL %s %s shape=%s alive=true", c.version, c.mode, shape)
		}
	}
	duration := 3 * time.Second
	if variant == "long" {
		duration = 60 * time.Second
	}
	if left := duration - time.Since(down); left > 0 {
		time.Sleep(left)
	}
	ready := server.start(newServer)
	matrixRequire(t, upgradeVersion(t, server.dir) == 4, "real migration v3 to v4")
	observer = matrixHTTPPeer(t, browser.origin, peerToken)
	queued = broker.Session{} // Zero omitted JSON fields instead of retaining the old pending count.
	matrixRequire(t, observer.tool(t.Context(), "get_session", map[string]string{"name": "peer"}, &queued) && queued.PendingMessages == 0, "pre-restart queue dropped")
	// Only an independent HTTP observer polls here: never stimulate stdio clients.
	deadline := ready.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, c := range connections {
			if c.recovered > 0 {
				continue
			}
			var s broker.Session
			if observer.tool(t.Context(), "get_session", map[string]string{"name": c.name}, &s) && string(s.Status) == "alive" && s.AutomaticDeliveryReady && s.RegistrationID == c.before.RegistrationID && s.Name == c.before.Name && s.Generation > c.before.Generation {
				c.recovered = time.Since(ready).Seconds()
			} else {
				all = false
			}
		}
		if all {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, c := range connections {
		t.Logf("RECOVERY %s %s %s seconds=%.3f alive=%t", variant, c.version, c.mode, c.recovered, upgradeAlive(c))
		matrixRequire(t, c.recovered > 0 && upgradeAlive(c), "unstimulated same-registration recovery")
	}
	var resumed broker.RegisterResult
	matrixRequire(t, observer.tool(t.Context(), "resume_session", broker.ResumeRequest{RegistrationID: peer.Session.RegistrationID, Harness: "codex", SessionID: matrixPeerSID, PID: os.Getpid(), CWD: server.dir, ClientID: "upgrade-peer-client", ChannelID: "peer-new", Secret: secret, RequestID: "peer-resume", ExpectedRevision: peer.Session.Revision}, &resumed) && resumed.Code == "", "peer resume after queue reset")
	for _, c := range connections {
		var own broker.Session
		matrixRequire(t, c.client.tool(t.Context(), "get_session", map[string]string{"name": c.name}, &own) && own.RegistrationID == c.before.RegistrationID, "first tool after recovery")
		matrixRequire(t, c.client.tool(t.Context(), "list_sessions", map[string]any{}, nil), "post-upgrade list")
		matrixRequire(t, upgradeHasAuth(t, c.client) == c.authSupported, "unchanged auth tool availability")
		matrixRequire(t, upgradeProof(t, c.auth) == c.proof, "unchanged credential and device requester")
		if c.authSupported {
			var auth struct {
				Status               string
				Registered           bool
				UserCode, DeviceCode string
			}
			matrixRequire(t, c.client.tool(t.Context(), "auth_status", map[string]any{}, &auth) && auth.Registered && auth.Status == "authorized" && auth.UserCode == "" && auth.DeviceCode == "", "authorized without code")
		}
		var renew broker.RegisterResult
		matrixRequire(t, observer.tool(t.Context(), "renew_lease", broker.RenewRequest{RegistrationID: peer.Session.RegistrationID, ChannelID: "peer-new"}, &renew) && renew.Code == "", "observer peer heartbeat")
		body := "after-" + c.version + "-" + c.mode
		matrixRequire(t, c.client.tool(t.Context(), "send_message", broker.Message{From: c.name, To: "peer", Body: body}, &sent) && sent.Accepted, "client to peer")
		var received broker.Message
		matrixRequire(t, observer.tool(t.Context(), "wait_for_message", map[string]string{"recipient": "peer", "channelId": "peer-new"}, &received) && received.Body == body, "peer receives after restart")
		if c.inbox != nil {
			matrixRequire(t, observer.tool(t.Context(), "send_message", broker.Message{From: "peer", To: c.name, Body: body}, &sent) && sent.Accepted, "peer to Claude")
			c.inbox.expect(t, "peer", c.name, body)
			matrixRequire(t, upgradeClaudeObservations(t, c.home) >= c.observations && upgradeAlive(c), "Claude inspected parent remains alive")
			var still broker.Session
			matrixRequire(t, observer.tool(t.Context(), "get_session", map[string]string{"name": c.name}, &still) && still.RegistrationID == c.before.RegistrationID && still.AutomaticDeliveryReady && string(still.Status) == "alive", "Claude parent liveness retains registration")
			t.Logf("CLAUDE_INBOX %s %s %s native-frame=true auth-frame=true parent-observed=true registration-retained=true", variant, c.version, c.mode)
		} else {
			matrixRequire(t, c.client.tool(t.Context(), "ingress_hook", map[string]string{"session_id": c.sid, "hook_event_name": "PreToolUse"}, nil), "synthetic active turn")
			matrixRequire(t, observer.tool(t.Context(), "send_message", broker.Message{From: "peer", To: c.name, Body: body}, &sent) && sent.Accepted, "peer to client")
			matrixWait(t, "client receives after restart", func() bool {
				var hook map[string]any
				if !c.client.tool(t.Context(), "ingress_hook", map[string]string{"session_id": c.sid, "hook_event_name": "PostToolUse"}, &hook) {
					return false
				}
				raw, _ := json.Marshal(hook)
				return strings.Contains(string(raw), body)
			})
		}
		t.Logf("UPGRADE_MATRIX %s %s %s PASS recovery=%.3fs same-id/name=true stdio-alive=true first-tool=true no-approval=true bidirectional=true queued-dropped=true down=%.3fs", variant, c.version, c.mode, c.recovered, time.Since(down).Seconds()-time.Since(ready).Seconds())
	}
	afterHashes, afterRequests := upgradeControlProof(t, server.dir)
	matrixRequire(t, maps.Equal(beforeHashes, afterHashes) && maps.Equal(beforeRequests, afterRequests), "no token replacement or new device request")
	t.Logf("UPGRADE_CONTROL %s unchanged-token-hashes=true new-device-requests=0 post-upgrade-approvals=0", variant)
}
