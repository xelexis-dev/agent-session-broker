package controlpanel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

// Only stopped, synthetic stores are copied; no live server data is an input.
func TestConnectReal132Rollback(t *testing.T) {
	binary := os.Getenv("ASB_CONNECT_OLD_SERVER")
	if binary == "" {
		if os.Getenv("ASB_CONNECT_GATE") == "1" {
			t.Fatal("gate requires ASB_CONNECT_OLD_SERVER")
		}
		t.Skip("opt-in real 1.3.2 rollback; ASB_CONNECT_GATE=1 requires all evidence")
	}
	version, err := exec.CommandContext(t.Context(), binary, "--version").Output()
	if err != nil || string(bytes.TrimSpace(version)) != "asb-server 1.3.2" {
		t.Fatal("old server must report 1.3.2")
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal("old server build info missing")
	}
	revision, clean := "", false
	buildSettings := map[string]string{}
	for _, setting := range info.Settings {
		buildSettings[setting.Key] = setting.Value
		if setting.Key == "vcs.revision" {
			revision = setting.Value
		}
		if setting.Key == "vcs.modified" {
			clean = setting.Value == "false"
		}
	}
	if revision != "e1d3b28dc30530d03c7abd806a3ffefa7c59ab62" || !clean {
		t.Fatal("old server must come from clean 1.3.2 tag")
	}
	if os.Getenv("ASB_CONNECT_GATE") == "1" {
		rollbackRequire(t, info.GoVersion == "go1.27.4", "old server pinned Go 1.27.4")
		rollbackRequire(t, buildSettings["CGO_ENABLED"] == "0", "old server pinned CGO_ENABLED=0")
		rollbackRequire(t, buildSettings["-trimpath"] == "true", "old server pinned trimpath")
	}
	data, err := os.ReadFile(binary)
	rollbackRequire(t, err == nil, "old binary digest")
	t.Logf("old server tag=%s sha256=%x toolchain=%s", revision, sha256.Sum256(data), info.GoVersion)
	f := rollbackFixture(t)
	before := rollbackFacts(t, f.dir)

	for _, key := range []string{"connect-manual", "connect-auto-approved", "connect-expired", "connect-revoked"} {
		found := false
		for _, device := range before.projects["p1"].Devices {
			if device.GrantID == f.ids[key] {
				found = true
			}
		}
		rollbackRequire(t, found, "HTTP fixture device authorization persisted "+key)
	}
	for _, key := range []string{"connect-issued", "connect-claiming", "connect-confirmed"} {
		found := false
		for _, exchange := range before.projects["p1"].AutoExchanges {
			if exchange.GrantID == f.ids[key] {
				found = true
			}
		}
		rollbackRequire(t, found, "HTTP fixture auto exchange persisted "+key)
	}
	candidate := rollbackCandidate(t)
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			copied := rollbackCopy(t, f.dir)
			if !enabled {
				store, err := OpenSQLite(filepath.Join(copied, "control"))
				rollbackRequire(t, err == nil, "OFF copy open")
				rollbackRequire(t, store.setConnectPolicy("admin", false) == nil, "OFF copy switch")
				rollbackRequire(t, store.Close() == nil, "OFF copy close")
			}
			baseline := rollbackFacts(t, copied)
			server := rollbackStart(t, binary, copied)
			server.rejectConnect(t, f)
			server.projectChecks(t, f, "old")
			server.stop()
			downgraded := rollbackFacts(t, copied)
			rollbackPreserved(t, baseline, downgraded)
			for _, key := range []string{"connect-issued", "connect-claiming", "connect-confirmed"} {
				rollbackRequire(t, downgraded.tokens[f.ids[key]].Revoked, "old commit permanently revokes "+key)
			}
			rollbackRequire(t, !downgraded.tokens[f.ids["connect-manual"]].Revoked && !downgraded.tokens[f.ids["connect-auto-approved"]].Revoked, "old commit keeps approved grants")
			upgraded := rollbackStart(t, candidate, copied)
			upgraded.projectChecks(t, f, "upgraded")
			upgraded.stop()
			after := rollbackFacts(t, copied)
			rollbackPreserved(t, downgraded, after)
			store, err := OpenSQLite(filepath.Join(copied, "control"))
			rollbackRequire(t, err == nil, "new store reopens after upgraded server")
			defer store.Close()
			rollbackRequire(t, store.connect.enabled == enabled, "connect switch preserved")
			for _, key := range []string{"connect-manual", "connect-auto-approved"} {
				retained, exists := after.tokens[f.ids[key]]
				rollbackRequire(t, exists && !retained.Revoked, "valid approved child retained")
				original := baseline.tokens[f.ids[key]]
				rollbackRequire(t, retained.ParentID == original.ParentID && retained.OwnerID == original.OwnerID && retained.Nickname == original.Nickname && retained.BoundIdentity == original.BoundIdentity && retained.TokenHash == original.TokenHash && retained.ExpiresAt.Equal(original.ExpiresAt) && reflect.DeepEqual(retained.Scopes, original.Scopes), "approved child identity, lifetime and scope preserved")
				rollbackRequire(t, store.Authorize("p1", f.children[key]) == enabled, "approved child eligibility follows preserved switch")
			}
			for _, key := range []string{"connect-issued", "connect-claiming", "connect-confirmed", "connect-expired", "connect-revoked"} {
				rollbackRequire(t, !store.Authorize("p1", f.children[key]), "ineligible child never resurrects "+key)
				if key != "connect-expired" {
					rollbackRequire(t, after.tokens[f.ids[key]].Revoked, "revoked child state retained")
				}
			}
			t.Log("criteria 3/4/6/7: user-parent children 401; old commit revokes auto; candidate starts; rows/audit/settings and approved children survive; no revoked/expired resurrection; reserved name blocks old approval")
		})
	}
	copied := rollbackCopy(t, f.dir)
	server := rollbackStart(t, binary, copied)
	server.login(t)
	server.browser(t, "/profile/api-tokens/revoke-all", "/profile/api-tokens", url.Values{})
	server.stop()
	revoked := rollbackFacts(t, copied)
	for key := range revoked.rows {
		rollbackRequire(t, !strings.HasPrefix(key, "user-tokens/admin/"), "old revoke-all deletes connect and management rows")
	}
	rollbackRequire(t, reflect.DeepEqual(before.users, revoked.users), "revoke-all preserves users")
	for key, raw := range before.rows {
		if strings.HasPrefix(key, "user-tokens/other/") {
			rollbackRequire(t, bytes.Equal(raw, revoked.rows[key]), "revoke-all preserves other account")
		}
	}
	upgraded := rollbackStart(t, candidate, copied)
	upgraded.stop()
	store, err := OpenSQLite(filepath.Join(copied, "control"))
	rollbackRequire(t, err == nil, "revoke-all reupgrade open")
	defer store.Close()
	for key, raw := range f.children {
		if strings.HasPrefix(key, "connect-") {
			rollbackRequire(t, !store.Authorize("p1", raw), "revoke-all child stays denied after upgrade")
		}
	}
	t.Log("criterion 5: real 1.3.2 user revoke-all deletes connect rows; other account preserved; reupgrade does not restore children")
}

func rollbackRequire(t *testing.T, ok bool, step string) {
	t.Helper()
	if !ok {
		t.Fatal(step)
	}
}

type rollbackSeed struct {
	dir           string
	parents       map[string]string
	children      map[string]string
	requests      map[string]DeviceRequest
	registrations map[string]protocol.RegisterResult
	ids           map[string]string
}

func rollbackFixture(t *testing.T) rollbackSeed {
	t.Helper()
	// The external production binary cannot read TestMain's fast KDF verifiers.
	previousKDF := passwordKDF
	passwordKDF = productionPasswordKDF
	defer func() { passwordKDF = previousKDF }()
	f := rollbackSeed{dir: t.TempDir(), parents: map[string]string{}, children: map[string]string{}, requests: map[string]DeviceRequest{}, ids: map[string]string{}, registrations: map[string]protocol.RegisterResult{}}
	s, err := OpenSQLite(filepath.Join(f.dir, "control"))
	rollbackRequire(t, err == nil, "new fixture store")
	t.Cleanup(func() { _ = s.Close() })
	rollbackRequire(t, s.Setup(testPassword) == nil, "fixture setup")
	rollbackRequire(t, s.SetInstanceHTTPPolicy("admin", true) == nil, "fixture http policy")
	s.data.ScopeIssuanceReady = true
	for i, project := range []string{"p1", "p2"} {
		rollbackRequire(t, s.CreateProject(project) == nil, "fixture project")
		for j, mode := range []string{"manual", "auto"} {
			key := project + "-" + mode
			scopes := access.LegacyV1Scopes()
			if mode == "auto" {
				scopes = append(scopes, access.SessionRegisterAuto)
			}
			parent, err := s.IssueProjectTokenWithOptionsFor("admin", project, key, ProjectTokenOptions{Scopes: scopes})
			rollbackRequire(t, err == nil, "fixture project token")
			f.parents[key] = parent
			req := DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: fmt.Sprintf("11111111-1111-4111-8111-%012d", 100+i*10+j), InstanceID: "fixture-" + key, RequestID: key}
			f.requests[key] = req
			if mode == "manual" {
				code, err := s.BeginDeviceAuthorization(project, parent, req)
				rollbackRequire(t, err == nil, "fixture device request")
				_, err = s.ApproveDeviceAuthorization("admin", code.UserCode, key, false)
				rollbackRequire(t, err == nil, "fixture approval")
				grant, err := s.ExchangeDeviceToken(project, parent, code.DeviceCode, req.ClientID, false)
				rollbackRequire(t, err == nil, "fixture approved exchange")
				f.children[key] = grant.AccessToken
			} else {
				grant, err := s.ExchangeDirect(project, parent, DirectRequest{DeviceRequest: req, RequestedNickname: key}, DirectProof{}, "synthetic", false)
				rollbackRequire(t, err == nil, "fixture auto exchange")
				f.children[key] = grant.AccessToken
			}
		}
	}
	rollbackRequire(t, s.SetAdminAPI("admin", true) == nil, "fixture management api")
	_, _, err = s.IssueOwnUserToken("admin", s.data.Users["admin"].Revision, "management", 3600, []string{"read"}, "synthetic", false)
	rollbackRequire(t, err == nil, "fixture management token")
	rollbackRequire(t, s.CreateUser("admin", "other", testPassword, "user") == nil, "other account")
	_, _, err = s.IssueOwnUserToken("other", s.data.Users["other"].Revision, "other management", 3600, []string{"read"}, "synthetic-other", false)
	rollbackRequire(t, err == nil, "other management fixture")
	rollbackConnectFixture(t, s, &f)
	rollbackRequire(t, s.Close() == nil, "fixture close")
	return f
}

func rollbackCopy(t *testing.T, source string) string {
	t.Helper()
	dest := t.TempDir()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("nonregular fixture entry")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0600)
	})
	rollbackRequire(t, err == nil, "stopped fixture copy")
	return dest
}

type rollbackServer struct {
	t       *testing.T
	origin  string
	client  *http.Client
	cmd     *exec.Cmd
	done    chan error
	stopped bool
}

func rollbackStart(t *testing.T, binary, dir string) *rollbackServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	rollbackRequire(t, err == nil, "ephemeral loopback port")
	address := listener.Addr().String()
	rollbackRequire(t, listener.Close() == nil, "release fixture port")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	s := &rollbackServer{t: t, origin: "http://" + address, client: &http.Client{Timeout: 5 * time.Second}, done: make(chan error, 1)}
	s.cmd = exec.CommandContext(ctx, binary, "--memory-dir", dir, "--listen", address)
	// Process output can contain setup codes; only named assertions reach test logs.
	s.cmd.Stdout, s.cmd.Stderr = io.Discard, io.Discard
	rollbackRequire(t, s.cmd.Start() == nil, "old server launch")
	go func() { s.done <- s.cmd.Wait() }()
	t.Cleanup(s.stop)
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			s.stopped = true
			t.Fatal("old server exited before readiness")
		case <-deadline.C:
			t.Fatal("old server readiness watchdog")
		case <-ticker.C:
			response, err := s.client.Get(s.origin + "/healthz")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == 200 {
					return s
				}
			}
		}
	}
}

func (s *rollbackServer) stop() {
	if s.stopped {
		return
	}
	s.stopped = true
	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case err := <-s.done:
		rollbackRequire(s.t, err == nil, "old server clean shutdown")
	case <-time.After(8 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
		s.t.Error("old server shutdown watchdog")
	}
}

func (s *rollbackServer) request(t *testing.T, path, bearer, content string, body []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), "POST", s.origin+path, bytes.NewReader(body))
	rollbackRequire(t, err == nil, "fixture request")
	request.Header.Set("Content-Type", content)
	request.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := s.client.Do(request)
	rollbackRequire(t, err == nil, "fixture http transport")
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	rollbackRequire(t, err == nil, "fixture http read")
	return response.StatusCode, raw
}

func (s *rollbackServer) form(t *testing.T, path, bearer string, fields url.Values, out any) {
	t.Helper()
	status, raw := s.request(t, path, bearer, "application/x-www-form-urlencoded", []byte(fields.Encode()))
	rollbackRequire(t, status == 200, "fixture oauth status")
	if out != nil {
		rollbackRequire(t, json.Unmarshal(raw, out) == nil, "fixture oauth decode")
	}
}

func (s *rollbackServer) browser(t *testing.T, path, page string, fields url.Values, expected ...int) {
	t.Helper()
	response, err := s.client.Get(s.origin + page)
	rollbackRequire(t, err == nil, "fixture browser page")
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	response.Body.Close()
	rollbackRequire(t, err == nil && response.StatusCode == 200, "fixture browser read")
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(raw)
	rollbackRequire(t, len(match) == 2, "fixture csrf")
	fields.Set("csrf", html.UnescapeString(string(match[1])))
	status, _ := s.request(t, path, "", "application/x-www-form-urlencoded", []byte(fields.Encode()))
	want := 303
	if len(expected) > 0 {
		want = expected[0]
	}
	rollbackRequire(t, status == want, fmt.Sprintf("fixture browser commit %s status=%d want=%d", path, status, want))
}

func (s *rollbackServer) rpc(t *testing.T, project, token, method string, params, out any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	rollbackRequire(t, err == nil, "fixture rpc marshal")
	status, raw := s.request(t, "/mcp?project="+project, token, "application/json", raw)
	rollbackRequire(t, status == 200, "fixture rpc status")
	var envelope struct {
		Result json.RawMessage
		Error  json.RawMessage
	}
	rollbackRequire(t, json.Unmarshal(raw, &envelope) == nil && len(envelope.Error) == 0, "fixture rpc envelope")
	rollbackRequire(t, json.Unmarshal(envelope.Result, out) == nil, "fixture rpc decode")
}

func (s *rollbackServer) tool(t *testing.T, project, token, name string, args, out any) {
	t.Helper()
	var result struct {
		IsError           bool
		StructuredContent json.RawMessage
		Content           []struct{ Text string }
	}
	s.rpc(t, project, token, "tools/call", map[string]any{"name": name, "arguments": args, "_meta": map[string]any{"io.modelcontextprotocol/clientInfo": map[string]string{"name": "agent-session-broker-client", "version": "1.3.0"}}}, &result)
	rollbackRequire(t, !result.IsError, "fixture tool success")
	raw := result.StructuredContent
	if len(raw) == 0 && len(result.Content) > 0 {
		raw = []byte(result.Content[0].Text)
	}
	rollbackRequire(t, json.Unmarshal(raw, out) == nil, "fixture tool decode")
}

func (s *rollbackServer) projectChecks(t *testing.T, f rollbackSeed, phase string) {
	t.Helper()
	s.login(t)
	for _, project := range []string{"p1", "p2"} {
		for i, mode := range []string{"manual", "auto"} {
			key := project + "-" + mode
			var code DeviceResponse
			sid := fmt.Sprintf("33333333-3333-4333-8333-%012d", i)
			if phase == "upgraded" {
				sid = fmt.Sprintf("44444444-4444-4444-8444-%012d", i)
			}
			fields := deviceForm(sid, phase+"-exchange-"+key)
			fields.Set("harness", "codex")
			fields.Set("asb_client_instance_id", phase+"-"+key)
			s.form(t, "/oauth/device_authorization?project="+project, f.parents[key], fields, &code)
			if project == "p1" && mode == "manual" && phase == "old" {
				s.browser(t, "/auth/code", "/auth/code", url.Values{"action": {"approve"}, "code": {code.UserCode}, "nickname": {"connect-manual"}}, 409)
			}
			s.browser(t, "/auth/code", "/auth/code", url.Values{"action": {"approve"}, "code": {code.UserCode}, "nickname": {phase + "-" + key}})
			var grant DeviceTokenResponse
			s.form(t, "/oauth/token?project="+project, f.parents[key], tokenForm(code.DeviceCode), &grant)
			rollbackRequire(t, strings.HasPrefix(grant.AccessToken, "asb_s_"), phase+" original project parent exchange")
			req := f.requests[key]
			channel := phase + "-" + key
			var result protocol.RegisterResult
			if phase == "old" {
				s.tool(t, project, f.children[key], "register_session", protocol.RegisterRequest{Harness: req.Harness, SessionID: req.SessionID, ClientID: req.InstanceID, PID: os.Getpid(), CWD: "/synthetic", ChannelID: channel, Secret: strings.Repeat("s", 40), RequestID: channel}, &result)
				rollbackRequire(t, result.Code == "" && result.Session.RegistrationID != "", "old project child registration")
				f.registrations[key] = result
			} else {
				previous := f.registrations[key]
				s.tool(t, project, f.children[key], "resume_session", protocol.ResumeRequest{RegistrationID: previous.Session.RegistrationID, Harness: req.Harness, SessionID: req.SessionID, ClientID: req.InstanceID, PID: os.Getpid(), CWD: "/synthetic", ChannelID: channel, Secret: strings.Repeat("s", 40), RequestID: channel, ExpectedRevision: previous.Session.Revision}, &result)
				rollbackRequire(t, result.Code == "" && result.Session.RegistrationID == previous.Session.RegistrationID, "upgraded original project session resumes same registration")
			}
			var renewed protocol.RegisterResult
			s.tool(t, project, f.children[key], "renew_lease", protocol.RenewRequest{RegistrationID: result.Session.RegistrationID, ChannelID: channel}, &renewed)
			if renewed.Code != "" {
				code := "other"
				switch renewed.Code {
				case "invalid", "not-found", "detached", "suspended", "owned", "revision-mismatch", "stale-probe", "storage", "capacity", "invalid-token", "registration-in-use", "probe-required", "ingress-pending":
					code = renewed.Code
				}
				t.Fatalf("project child renew phase=%s project=%s mode=%s code=%s", phase, project, mode, code)
			}
		}
	}
	t.Logf("phase=%s: P1/P2 original manual/auto project parents exchange; original sessions register/resume and renew", phase)
}

func (s *rollbackServer) login(t *testing.T) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	rollbackRequire(t, err == nil, "fixture cookies")
	s.client.Jar = jar
	s.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s.browser(t, "/login", "/login", url.Values{"username": {"admin"}, "password": {testPassword}})
}

func rollbackConnectFixture(t *testing.T, s *Store, f *rollbackSeed) {
	t.Helper()
	rollbackRequire(t, s.setConnectPolicy("admin", true) == nil, "connect switch ON")
	manualRaw, manual, err := s.issueConnectToken("admin", 0, connectOptions{AdmissionMode: "manual"})
	rollbackRequire(t, err == nil, "manual connect parent")
	refs := []connectProject{}
	for _, name := range []string{"p1", "p2"} {
		refs = append(refs, connectProject{Name: name, Generation: s.data.Projects[name].ProjectGenerationID})
	}
	autoRaw, auto, err := s.issueConnectToken("admin", 0, connectOptions{AdmissionMode: "auto", AutoProjects: refs})
	rollbackRequire(t, err == nil, "auto connect parent")
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	httpFixture := &deviceHTTPFixture{t: t, store: s, h: h}
	owner := loginPanelUser(t, h, "admin")
	for i, key := range []string{"connect-manual", "connect-auto-approved", "connect-issued", "connect-claiming", "connect-confirmed", "connect-expired", "connect-revoked"} {
		parent, parentRaw := manual, manualRaw
		if i >= 1 && i <= 4 {
			parent, parentRaw = auto, autoRaw
		}
		req := DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: fmt.Sprintf("22222222-2222-4222-8222-%012d", i+100), InstanceID: "fixture-" + key, RequestID: key}
		var response DeviceTokenResponse
		if i >= 2 && i <= 4 {
			form := url.Values{"client_id": {req.ClientID}, "harness": {req.Harness}, "session_id": {req.SessionID}, "asb_client_instance_id": {req.InstanceID}, "request_id": {req.RequestID}, "requested_nickname": {key}}
			w, _ := httpFixture.rest("POST", directPath+"?project=p1", parentRaw, form)
			rollbackRequire(t, w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &response) == nil, "HTTP fixture direct registration")
		} else {
			form := deviceForm(req.SessionID, req.RequestID)
			form.Set("harness", req.Harness)
			form.Set("asb_client_instance_id", req.InstanceID)
			w, _ := httpFixture.rest("POST", deviceAuthorizationPath+"?project=p1", parentRaw, form)
			var code DeviceResponse
			rollbackRequire(t, w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &code) == nil, "HTTP fixture device authorization")
			approved := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {code.UserCode}, "nickname": {key}})
			rollbackRequire(t, approved.Code == 303, "HTTP fixture panel approval")
			w, _ = httpFixture.rest("POST", deviceTokenPath+"?project=p1", parentRaw, tokenForm(code.DeviceCode))
			rollbackRequire(t, w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &response) == nil, "HTTP fixture token exchange")
		}
		rollbackRequire(t, strings.HasPrefix(response.AccessToken, "asb_s_") && response.Nickname == key && response.ParentKind == "user-connect" && response.ParentExpiresAt.Equal(parent.ExpiresAt) && response.Project == "p1" && response.ProjectGeneration == s.ProjectGeneration("p1"), "HTTP fixture connect metadata")
		f.children[key], f.ids[key], f.requests[key] = response.AccessToken, response.GrantID, req
		// Exercise the normal broker confirmation phases to retain crash-recovery states.
		if i == 3 || i == 4 {
			grant, _, ok := s.authorize("p1", response.AccessToken)
			rollbackRequire(t, ok && grant.Registration != nil, "HTTP fixture direct grant")
			registration := "synthetic-registration-" + key
			rollbackRequire(t, grant.Registration(req.identity(), registration, "", key, 0, false) == nil, "fixture claiming phase")
			if i == 4 {
				rollbackRequire(t, grant.Registration(req.identity(), registration, "admin", key, 1, true) == nil, "fixture confirmed phase")
			}
		}
	}
	// Age only the expired fixture after normal HTTP creation; production time is unchanged.
	p := s.data.Projects["p1"]
	p.Tokens = append([]SessionToken(nil), p.Tokens...)
	now := s.now().UTC()
	for i := range p.Tokens {
		if p.Tokens[i].ID == f.ids["connect-expired"] {
			p.Tokens[i].IssuedAt = now.Add(-2 * time.Hour)
			p.Tokens[i].ExpiresAt = now.Add(-time.Hour)
		}
	}
	rollbackRequire(t, s.commitProject(p) == nil, "HTTP fixture expiry seed")
	rollbackRequire(t, s.Authorize("p1", f.children["connect-revoked"]), "designated revoked fixture starts eligible")
	rollbackRequire(t, s.RevokeTokenFor("admin", "p1", f.ids["connect-revoked"]) == nil, "normal connect child revocation")
	rollbackRequire(t, !s.Authorize("p1", f.children["connect-revoked"]), "normal revocation denies designated child")
	rollbackRequire(t, len(s.connect.audits) > 0 && len(s.audits) > 0, "both audit namespaces seeded")
	for _, key := range []string{"connect-manual", "connect-auto-approved", "connect-issued", "connect-claiming", "connect-confirmed"} {
		rollbackRequire(t, s.Authorize("p1", f.children[key]), "new store accepts eligible HTTP connect fixture "+key)
	}
}

func (s *rollbackServer) rejectConnect(t *testing.T, f rollbackSeed) {
	t.Helper()
	for key, raw := range f.children {
		if !strings.HasPrefix(key, "connect-") {
			continue
		}
		status, _ := s.request(t, "/mcp?project=p1", raw, "application/json", []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"agent-session-broker-client","version":"1.3.0"}}}`))
		rollbackRequire(t, status == 401, "old server rejects user-parent child "+key)
	}
}

type rollbackSnapshot struct {
	projects map[string]Project
	rows     map[string][]byte
	users    map[string]userRecord
	tokens   map[string]SessionToken
	files    map[string]string
}

func rollbackFacts(t *testing.T, dir string) rollbackSnapshot {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(dir, "control"), "control.db")
	rollbackRequire(t, err == nil, "logical control open")
	rows, err := db.List()
	rollbackRequire(t, err == nil, "logical control read")
	rollbackRequire(t, db.Close() == nil, "logical control close")
	var st state
	rollbackRequire(t, json.Unmarshal(rows["state"], &st) == nil, "logical state decode")
	out := rollbackSnapshot{projects: st.Projects, rows: rows, users: st.Users, tokens: map[string]SessionToken{}, files: map[string]string{}}
	for _, project := range st.Projects {
		for _, token := range project.Tokens {
			out.tokens[token.ID] = token
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "control"))
	rollbackRequire(t, err == nil, "control files list")
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "control.db") || strings.Contains(entry.Name(), "lock") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "control", entry.Name()))
		rollbackRequire(t, err == nil, "control file read")
		out.files[entry.Name()] = fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	return out
}

func rollbackPreserved(t *testing.T, before, after rollbackSnapshot) {
	t.Helper()
	rollbackRequire(t, reflect.DeepEqual(before.users, after.users), "user rows preserved logically")
	// Instance policy outside projects/users (scope issuance and its history, HTTP, unauthenticated access, admin, version) must survive too.
	var wantState, gotState map[string]any
	rollbackRequire(t, json.Unmarshal(before.rows["state"], &wantState) == nil && json.Unmarshal(after.rows["state"], &gotState) == nil, "state row decode")
	for _, key := range []string{"projects", "users"} {
		delete(wantState, key)
		delete(gotState, key)
	}
	rollbackRequire(t, reflect.DeepEqual(wantState, gotState), "instance policy and state metadata preserved")
	rollbackRequire(t, reflect.DeepEqual(before.files, after.files), "control side files preserved")
	rollbackRequire(t, len(before.projects) == len(after.projects), "project set preserved")
	rollbackRequire(t, len(after.tokens) == len(before.tokens)+4, "only four exercised project device grants are added")
	for name, want := range before.projects {
		got, exists := after.projects[name]
		rollbackRequire(t, exists, "project retained")
		// OAuth exercises add device/grant records and advance auto registration state.
		want.Tokens, got.Tokens = nil, nil
		want.Devices, got.Devices = nil, nil
		want.AutoBindings, got.AutoBindings = nil, nil
		want.AutoExchanges, got.AutoExchanges = nil, nil
		rollbackRequire(t, reflect.DeepEqual(want, got), "project policy, generation and membership preserved")
	}
	projectParents := map[string]bool{}
	for id, token := range before.tokens {
		if token.Kind == tokenProject {
			projectParents[id] = true
		}
	}
	for name, p := range before.projects {
		for _, device := range p.Devices {
			if projectParents[device.TokenID] {
				continue
			}
			found := false
			for _, got := range after.projects[name].Devices {
				if got.CodeHash == device.CodeHash && reflect.DeepEqual(device, got) {
					found = true
				}
			}
			rollbackRequire(t, found, "HTTP user-parent device row preserved")
		}
		for _, exchange := range p.AutoExchanges {
			if projectParents[exchange.ParentID] {
				continue
			}
			found := false
			for _, got := range after.projects[name].AutoExchanges {
				if got.ParentID == exchange.ParentID && got.RequestID == exchange.RequestID && reflect.DeepEqual(exchange, got) {
					found = true
				}
			}
			rollbackRequire(t, found, "HTTP user-parent auto exchange preserved")
		}
	}
	for id, want := range before.tokens {
		if !projectParents[id] && !projectParents[want.ParentID] {
			continue
		}
		got, exists := after.tokens[id]
		rollbackRequire(t, exists, "original project token/session retained")
		// Only this test's approvals and register/resume/renew touches may mutate these fields.
		rollbackRequire(t, got.Approvals >= want.Approvals && got.Approvals <= want.Approvals+1, "project approval count only advances once")
		for _, times := range [][2]time.Time{{want.FirstUsedAt, got.FirstUsedAt}, {want.LastUsedAt, got.LastUsedAt}, {want.FirstConnectedAt, got.FirstConnectedAt}, {want.LastRegisteredAt, got.LastRegisteredAt}} {
			rollbackRequire(t, times[0].IsZero() || !times[1].Before(times[0]), "project credential timestamps never regress")
		}
		want.Approvals, got.Approvals = 0, 0
		want.FirstUsedAt, got.FirstUsedAt = time.Time{}, time.Time{}
		want.LastUsedAt, got.LastUsedAt = time.Time{}, time.Time{}
		want.FirstConnectedAt, got.FirstConnectedAt = time.Time{}, time.Time{}
		want.LastRegisteredAt, got.LastRegisteredAt = time.Time{}, time.Time{}
		rollbackRequire(t, reflect.DeepEqual(want, got), "original project token/session identity, scopes, limits and revocation preserved")
	}
	for key, raw := range before.rows {
		if strings.HasPrefix(key, "user-tokens/") || strings.HasPrefix(key, "connect-audit/") || strings.HasPrefix(key, "audit/") || strings.HasPrefix(key, "settings/") {
			var want, got any
			rollbackRequire(t, json.Unmarshal(raw, &want) == nil && json.Unmarshal(after.rows[key], &got) == nil && reflect.DeepEqual(want, got), "control row preserved: "+strings.Split(key, "/")[0])
		}
	}
	for id, token := range before.tokens {
		if token.Revoked {
			rollbackRequire(t, after.tokens[id].Revoked, "prior revocation preserved")
		}
	}
}

func rollbackCandidate(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	rollbackRequire(t, ok, "candidate source location")
	binary := filepath.Join(t.TempDir(), "asb-server-candidate")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", binary, "./cmd/asb-server")
	cmd.Dir = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	rollbackRequire(t, cmd.Run() == nil, "candidate server build")
	return binary
}
