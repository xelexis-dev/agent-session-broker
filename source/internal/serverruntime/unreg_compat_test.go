package serverruntime

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

// Synthetic test credentials travel in process memory/environment, not config files.
func TestUnregCompatibilityParentHelper(t *testing.T) {
	raw := os.Getenv("ASB_UNREG_CHILD_CONFIG")
	if raw == "" {
		t.Skip("named synthetic parent only")
	}
	var c matrixChildConfig
	if json.Unmarshal([]byte(raw), &c) != nil {
		os.Exit(70)
	}
	cmd := exec.Command(c.Binary, "--broker-url", c.Endpoint, "--token", c.Token, "--auth-dir", c.AuthDir)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "ASB_UNREG_CHILD_CONFIG=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, io.Discard
	if cmd.Run() != nil {
		os.Exit(71)
	}
	os.Exit(0)
}
func unregRuntime(t *testing.T) (*deviceRuntime, string) {
	t.Helper()
	var parent string
	r, _ := newDeviceRuntime(t, func(s *controlpanel.Store) {
		matrixRequire(t, s.SetScopeIssuanceReady("admin", true) == nil && s.SetInstanceHTTPPolicy("admin", true) == nil, "unreg setup")
		var err error
		parent, err = s.IssueProjectTokenWithOptionsFor("admin", "test", "unreg-parent", controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
		matrixRequire(t, err == nil, "unreg parent")
	})
	return r, parent
}
func unregAuto(t *testing.T, r *deviceRuntime, parent string, n int, name string, capable bool) (string, protocol.RegisterResult) {
	t.Helper()
	request := protectedRequest(n, name)
	if capable {
		request.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
	}
	status, out := r.rest("/oauth/session_registration?project=test", parent, url.Values{"client_id": {"asb-client-v1"}, "harness": {request.Harness}, "session_id": {request.SessionID}, "asb_client_instance_id": {request.ClientID}, "request_id": {request.RequestID}, "requested_nickname": {name}})
	token, _ := out["access_token"].(string)
	matrixRequire(t, status == 200 && token != "", "unreg direct")
	registered := managedTool[protocol.RegisterResult](t, r.bound(token), "register_session", request)
	matrixRequire(t, registered.Code == "", "unreg register")
	return token, registered
}
func unregRecordCapability(t *testing.T, dir, id string) bool {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "projects", "p-*", "registrations", "registrations.db"))
	matrixRequire(t, len(files) == 1, "unreg registration DB")
	db, err := sqlitestore.Open(filepath.Dir(files[0]), "registrations.db")
	matrixRequire(t, err == nil, "unreg registration read")
	defer db.Close()
	raw, err := db.Get(id)
	matrixRequire(t, err == nil && len(raw) > 0, "unreg registration row")
	var row struct {
		Capable bool `json:"reattach_after_cleanup"`
	}
	matrixRequire(t, json.Unmarshal(raw, &row) == nil, "unreg registration decode")
	return row.Capable
}

func TestUnregCleanupU15Actual0172Rollback(t *testing.T) {
	binary := os.Getenv("ASB_UNREG_0172_SERVER")
	if binary == "" {
		t.Skip("opt-in: tag-built 0.17.2 server")
	}
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "policy-committed-record-retained"}[partial], func(t *testing.T) {
			r, parent := unregRuntime(t)
			token, out := unregAuto(t, r, parent, 1601, "rollback-worker", true)
			managedTool[any](t, r.bound(token), "stop_session", map[string]string{"name": out.Session.RegistrationID})
			files, _ := filepath.Glob(filepath.Join(r.dir, "projects", "p-*", "registrations", "registrations.db"))
			matrixRequire(t, len(files) == 1, "rollback records")
			if partial {
				matrixRequire(t, os.Chmod(filepath.Dir(files[0]), 0500) == nil, "partial delete fault")
			}
			page := r.browser("GET", "/projects/sessions?project=test", nil)
			matrixRequire(t, page.Code == 200 && r.mcpStatus(token) == 401, "current cleanup policy")
			if partial {
				matrixRequire(t, os.Chmod(filepath.Dir(files[0]), 0700) == nil, "partial permissions restored")
			}
			matrixRequire(t, r.close() == nil, "current runtime stopped")
			reserved, err := net.Listen("tcp", "127.0.0.1:0")
			matrixRequire(t, err == nil, "old port")
			address := reserved.Addr().String()
			reserved.Close()
			old := &upgradeServer{t: t, address: address, dir: r.dir}
			old.start(binary)
			defer old.stop()
			jar, _ := cookiejar.New(nil)
			browser := &upgradeBrowser{t: t, origin: "http://" + address, client: &http.Client{Jar: jar, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
			browser.post("/login", "/login", url.Values{"username": {"admin"}, "password": {r.password}})
			response, err := browser.client.Get(browser.origin + "/projects/sessions?project=test")
			matrixRequire(t, err == nil, "old list transport")
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			matrixRequire(t, response.StatusCode == 200 && !strings.Contains(string(body), "rollback-worker"), "old server reads cleanup stores/audit")
			t.Log("actual tag-built 0.17.2 reads control + registration stores and lists successfully")
		})
	}
}

func unregBinaryClient(t *testing.T, binary, helper, endpoint, parent, auth, home, cwd string) *matrixClient {
	t.Helper()
	raw, _ := json.Marshal(matrixChildConfig{Binary: binary, Endpoint: endpoint, Token: parent, AuthDir: auth})
	cmd := exec.Command(helper, "-test.run=^TestUnregCompatibilityParentHelper$")
	cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "CODEX_HOME=" + filepath.Join(home, ".codex"), "CODEX_THREAD_ID=" + matrixSID, "ASB_UNREG_CHILD_CONFIG=" + string(raw)}
	cmd.Dir = cwd
	cmd.Stderr = io.Discard
	session, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-unreg-compatibility", Version: "1"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}, nil)
	matrixRequire(t, err == nil, "unreg binary initialize")
	c := &matrixClient{session}
	t.Cleanup(c.close)
	return c
}

func TestUnregCleanupR5Actual0172ClientAndServer(t *testing.T) {
	newBinary, oldBinary, oldServer := os.Getenv("ASB_UNREG_CURRENT_CLIENT"), os.Getenv("ASB_UNREG_0172_CLIENT"), os.Getenv("ASB_UNREG_0172_SERVER")
	if newBinary == "" || oldBinary == "" || oldServer == "" {
		t.Skip("opt-in: current client and tag-built 0.17.2 client/server")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("named Codex parent fixture")
	}
	r, parent := unregRuntime(t)
	server := httptest.NewServer(r.h)
	defer server.Close()
	endpoint := server.URL + "/mcp?project=test"
	self, err := os.Executable()
	matrixRequire(t, err == nil, "test executable")
	raw, err := os.ReadFile(self)
	matrixRequire(t, err == nil, "parent helper bytes")
	helper := filepath.Join(t.TempDir(), "codex")
	matrixRequire(t, os.WriteFile(helper, raw, 0700) == nil, "parent helper copy")
	home, auth, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "10", "27")
	matrixRequire(t, os.MkdirAll(rollout, 0700) == nil, "synthetic rollout directory")
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": matrixSID, "cwd": cwd}})
	matrixRequire(t, os.WriteFile(filepath.Join(rollout, "rollout-synthetic-"+matrixSID+".jsonl"), append(meta, '\n'), 0600) == nil, "synthetic conversation identity")
	current := unregBinaryClient(t, newBinary, helper, endpoint, parent, auth, home, cwd)
	matrixRequire(t, current.tool(t.Context(), "attach_session", map[string]any{}, nil), "current binary attach")
	var status struct {
		Registered     bool
		RegistrationID string
	}
	matrixWait(t, "current registered", func() bool {
		return current.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Registered
	})
	matrixRequire(t, unregRecordCapability(t, r.dir, status.RegistrationID), "new holder capability true")
	current.close()
	files, _ := filepath.Glob(filepath.Join(auth, "sessions", "*", "conn.json"))
	matrixRequire(t, len(files) == 1, "real client state")
	data, err := os.ReadFile(files[0])
	matrixRequire(t, err == nil, "state read")
	var state clientstate.SessionState
	matrixRequire(t, json.Unmarshal(data, &state) == nil, "state decode")
	state.Direct.Reattach = true
	store, err := clientstate.Open(auth)
	matrixRequire(t, err == nil, "client state open")
	matrixRequire(t, store.SaveSession(filepath.Base(filepath.Dir(files[0])), state) == nil, "optional Reattach evidence")
	old := unregBinaryClient(t, oldBinary, helper, endpoint, parent, auth, home, cwd)
	matrixWait(t, "0.17.2 resumes same state on new server", func() bool {
		return old.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Registered
	})
	matrixRequire(t, !unregRecordCapability(t, r.dir, status.RegistrationID), "old holder clears capability")
	old.close()
	// Restore a capable current holder before the actual old-server reader/writer run.
	current = unregBinaryClient(t, newBinary, helper, endpoint, parent, auth, home, cwd)
	matrixWait(t, "current holder restored", func() bool {
		return current.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Registered
	})
	matrixRequire(t, unregRecordCapability(t, r.dir, status.RegistrationID), "current capability restored")
	current.close()
	address := server.Listener.Addr().String()
	server.Close()
	matrixRequire(t, r.close() == nil, "new runtime closed")
	legacy := &upgradeServer{t: t, address: address, dir: r.dir}
	legacy.start(oldServer)
	defer legacy.stop()
	old = unregBinaryClient(t, oldBinary, helper, endpoint, parent, auth, home, cwd)
	matrixWait(t, "0.17.2 client/server same-state resume", func() bool {
		return old.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Registered
	})
	matrixRequire(t, !unregRecordCapability(t, r.dir, status.RegistrationID), "old server rewrite drops extension")
	matrixRequire(t, old.tool(t.Context(), "stop_session", map[string]string{"name": status.RegistrationID}, nil), "old server stop")
	old.close()
	legacy.stop()
	t.Log("actual 0.17.2 client tolerates Reattach; old/new holder capability and actual old-server rewrite verified")
}
