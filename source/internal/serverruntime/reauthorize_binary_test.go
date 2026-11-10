package serverruntime

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Opt-in: both executables are locally built, all policy/registration/token data
// is disposable, and a named synthetic Codex parent never invokes a model.
func TestAutoReauthorizationRealBinaries(t *testing.T) {
	clientBinary, serverBinary := os.Getenv("ASB_REAUTH_CLIENT_BINARY"), os.Getenv("ASB_REAUTH_SERVER_BINARY")
	if clientBinary == "" || serverBinary == "" {
		t.Skip("opt-in: set locally built ASB_REAUTH_CLIENT_BINARY and ASB_REAUTH_SERVER_BINARY")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("named-parent fixture currently exercised on macOS")
	}
	dir := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	matrixRequire(t, err == nil, "reauth seed")
	matrixRequire(t, store.Setup("synthetic-upgrade-password") == nil && store.CreateProject("test") == nil, "reauth setup")
	matrixRequire(t, store.SetInstanceHTTPPolicy("admin", true) == nil && store.SetScopeIssuanceReady("admin", true) == nil, "reauth policy")
	options := controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)}
	parentA, err := store.IssueProjectTokenWithOptionsFor("admin", "test", "synthetic-parent-a", options)
	matrixRequire(t, err == nil, "parent a")
	parentB, err := store.IssueProjectTokenWithOptionsFor("admin", "test", "synthetic-parent-b", options)
	matrixRequire(t, err == nil, "parent b")
	matrixRequire(t, store.Close() == nil, "seed close")
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	matrixRequire(t, err == nil, "port reservation")
	address := reservation.Addr().String()
	_ = reservation.Close()
	server := &upgradeServer{t: t, address: address, dir: dir}
	server.start(serverBinary)
	defer server.stop()
	jar, _ := cookiejar.New(nil)
	browser := &upgradeBrowser{t: t, origin: "http://" + address, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	browser.login()
	self, _ := os.Executable()
	raw, err := os.ReadFile(self)
	matrixRequire(t, err == nil, "parent helper bytes")
	helper := filepath.Join(t.TempDir(), "codex")
	matrixRequire(t, os.WriteFile(helper, raw, 0700) == nil, "parent helper")
	home, authDir, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	sid := "11111111-1111-4111-8111-111111119593"
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "10", "18")
	matrixRequire(t, os.MkdirAll(rollout, 0700) == nil, "rollout directory")
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": sid, "cwd": cwd}})
	matrixRequire(t, os.WriteFile(filepath.Join(rollout, "rollout-synthetic-"+sid+".jsonl"), append(meta, '\n'), 0600) == nil, "rollout identity")
	backendURL, _ := url.Parse("http://" + address)
	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	var admissions, registers atomic.Int32
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/session_registration" || r.URL.Path == "/oauth/device_authorization" || r.URL.Path == "/oauth/token" {
			admissions.Add(1)
		}
		if r.Method == "POST" && r.URL.Path == "/mcp" {
			data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(data))
			var request struct {
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			_ = json.Unmarshal(data, &request)
			if request.Params.Name == "register_session" {
				registers.Add(1)
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()
	endpoint := front.URL + "/mcp?project=test"
	c := matrixStart(t, clientBinary, helper, endpoint, parentA, authDir, home, cwd, sid)
	defer func() { c.close() }()
	var status struct {
		Registered                    bool
		Name, RegistrationID, Message string
	}
	matrixRequire(t, c.tool(t.Context(), "auth_status", map[string]any{}, &status) && !status.Registered, "fresh binary stays unregistered")
	_ = c.tool(t.Context(), "list_sessions", map[string]any{}, nil)
	projectBefore, _ := autoClientEvidence(t, dir)
	children := 0
	for _, token := range projectBefore.Tokens {
		if token.ParentID != "" {
			children++
		}
	}
	matrixRequire(t, len(projectBefore.Devices) == 0 && children == 0 && admissions.Load() == 0 && registers.Load() == 0, "fresh binary performs no admission")
	matrixRequire(t, c.tool(t.Context(), "attach_session", map[string]any{}, nil), "explicit binary attach")
	t.Log("BINARY_EXPLICIT target=temporary-test-server startupRegistered=false diagnosticRegistered=false admissionsBeforeAttach=0 attachRequested=true PASS")
	matrixWait(t, "initial explicit auto-approved registration", func() bool { return c.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Registered })
	var renamed broker.Session
	matrixRequire(t, c.tool(t.Context(), "rename_session", map[string]string{"name": "reauth-worker"}, &renamed), "rename before revoke")
	originalID := status.RegistrationID
	readState := func() clientstate.SessionState {
		files, _ := filepath.Glob(filepath.Join(authDir, "sessions", "*", "conn.json"))
		matrixRequire(t, len(files) == 1, "client state file")
		data, e := os.ReadFile(files[0])
		var state clientstate.SessionState
		matrixRequire(t, e == nil && json.Unmarshal(data, &state) == nil && state.Credential != nil, "private client state")
		return state
	}
	state := readState()
	oldSource := state.AdmissionFingerprint
	oldGrant := state.Credential.SessionGrantID
	browser.post("/tokens/revoke", "/", url.Values{"project": {"test"}, "id": {oldGrant}})
	// No client tools/call until receiver/heartbeat has caused another direct grant.
	matrixWait(t, "receiver-only direct reauthorization", func() bool {
		project, _ := autoClientEvidence(t, dir)
		n := 0
		for _, token := range project.Tokens {
			if token.AdmissionMode == "auto" && token.ParentID != "" {
				n++
			}
		}
		return n >= 2
	})
	matrixWait(t, "same registration ready", func() bool {
		return c.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Registered && status.Name == "reauth-worker" && status.RegistrationID == originalID
	})
	state = readState()
	matrixRequire(t, state.Credential.SessionGrantID != oldGrant, "replacement grant persisted")
	t.Log("BINARY_REAUTH target=temporary-test-server receiver401=true toolsRequired=false sameRegistration=true sameRenamedName=true PASS")
	c.close()
	c = matrixStart(t, clientBinary, helper, endpoint, parentB, authDir, home, cwd, sid)
	matrixWait(t, "restart with new parent and valid old credential", func() bool { return c.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Registered })
	state = readState()
	matrixRequire(t, state.AdmissionFingerprint == oldSource, "valid credential retains original source")
	oldGrant = state.Credential.SessionGrantID
	browser.post("/tokens/revoke", "/", url.Values{"project": {"test"}, "id": {oldGrant}})
	matrixWait(t, "new parent source persisted after rejection", func() bool {
		files, _ := filepath.Glob(filepath.Join(authDir, "sessions", "*", "conn.json"))
		if len(files) != 1 {
			return false
		}
		data, e := os.ReadFile(files[0])
		var state clientstate.SessionState
		return e == nil && json.Unmarshal(data, &state) == nil && state.Credential != nil && state.Credential.SessionGrantID != oldGrant && state.AdmissionFingerprint != oldSource
	})
	matrixWait(t, "token change resumes same registration", func() bool {
		return c.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Registered && status.Name == "reauth-worker" && status.RegistrationID == originalID
	})
	project, _ := autoClientEvidence(t, dir)
	matrixRequire(t, len(project.Devices) == 0, "no device authorization fallback")
	t.Log("BINARY_REAUTH target=temporary-test-server tokenChanged=true validOldCredentialPreserved=true later401=true sameRegistration=true sameRenamedName=true devices=0 PASS")
}
