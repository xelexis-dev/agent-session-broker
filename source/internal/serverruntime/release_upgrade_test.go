package serverruntime

import (
	"bytes"
	"crypto/sha256"
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
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

type releaseTokenFact struct {
	Owner, Mode     string
	Scopes          []string
	Issued, Expires time.Time
	Cap             int
	Revoked         bool
}
type releaseRegistrationFact struct {
	ID        string `json:"registration_id"`
	Name      string `json:"name"`
	Owner     string `json:"owner_id"`
	Detached  bool   `json:"detached"`
	Suspended bool   `json:"suspended"`
}
type releaseFacts struct {
	Users map[string]struct {
		Role    string
		Enabled bool
	}
	Members       map[string]map[string]string
	Tokens        map[string]releaseTokenFact
	Registrations map[string]releaseRegistrationFact
	Settings      map[string]bool
	API           map[string]string
}

func releaseReadFacts(t *testing.T, root string) releaseFacts {
	t.Helper()
	out := releaseFacts{Users: map[string]struct {
		Role    string
		Enabled bool
	}{}, Members: map[string]map[string]string{}, Tokens: map[string]releaseTokenFact{}, Registrations: map[string]releaseRegistrationFact{}, Settings: map[string]bool{}, API: map[string]string{}}
	db, e := sqlitestore.Open(filepath.Join(root, "control"), "control.db")
	matrixRequire(t, e == nil, "invariant-control-open")
	defer db.Close()
	records, e := db.List()
	matrixRequire(t, e == nil, "invariant-control-read")
	var state struct {
		Users              map[string]controlpanel.User
		Projects           map[string]controlpanel.Project
		AllowHTTP          bool
		ScopeIssuanceReady bool
	}
	matrixRequire(t, json.Unmarshal(records["state"], &state) == nil, "invariant-state-decode")
	for id, u := range state.Users {
		out.Users[id] = struct {
			Role    string
			Enabled bool
		}{u.Role, u.Enabled}
	}
	for name, p := range state.Projects {
		out.Members[name] = p.Members
		for _, token := range p.Tokens {
			out.Tokens[token.ID] = releaseTokenFact{token.OwnerID, token.AdmissionMode, token.Scopes, token.IssuedAt, token.ExpiresAt, token.MaxAutoBindings, token.Revoked}
		}
	}
	out.Settings["http"] = state.AllowHTTP
	out.Settings["scopes"] = state.ScopeIssuanceReady
	for _, key := range []string{"settings/admin-api", "settings/attachments"} {
		if raw := records[key]; len(raw) > 0 {
			var setting struct{ Enabled bool }
			matrixRequire(t, json.Unmarshal(raw, &setting) == nil, "invariant-policy-decode")
			out.Settings[key] = setting.Enabled
		}
	}
	for key, raw := range records {
		if strings.HasPrefix(key, "user-tokens/") {
			var token map[string]json.RawMessage
			matrixRequire(t, json.Unmarshal(raw, &token) == nil, "invariant-api-token-decode")
			delete(token, "hash")
			delete(token, "last_used_at")
			data, _ := json.Marshal(token)
			out.API[key] = string(data)
		}
	}
	e = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "registrations.db" {
			return nil
		}
		db, err := sqlitestore.Open(filepath.Dir(path), entry.Name())
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.List()
		if err != nil {
			return err
		}
		for _, raw := range rows {
			var fact releaseRegistrationFact
			if json.Unmarshal(raw, &fact) != nil || fact.ID == "" {
				return fmt.Errorf("invariant-registration-decode")
			}
			out.Registrations[fact.ID] = fact
		}
		return nil
	})
	matrixRequire(t, e == nil, "invariant-registration-read")
	return out
}

func releaseCompare(want, got releaseFacts) string {
	if !reflect.DeepEqual(want.Users, got.Users) {
		return "users-preserved"
	}
	if !reflect.DeepEqual(want.Members, got.Members) {
		return "roles-preserved"
	}
	if len(want.Tokens) != len(got.Tokens) {
		return "tokens-preserved"
	}
	for id, w := range want.Tokens {
		g, ok := got.Tokens[id]
		if !ok {
			return "tokens-preserved"
		}
		if w.Revoked != g.Revoked {
			return "revocation-preserved"
		}
		if !reflect.DeepEqual(w, g) {
			return "scopes-lifetime-preserved"
		}
	}
	if !reflect.DeepEqual(want.Registrations, got.Registrations) {
		return "registrations-preserved"
	}
	if !reflect.DeepEqual(want.Settings, got.Settings) {
		return "settings-preserved"
	}
	if !reflect.DeepEqual(want.API, got.API) {
		return "api-tokens-preserved"
	}
	return ""
}

func TestReleaseReRevocationIsRequired(t *testing.T) {
	want := releaseFacts{Tokens: map[string]releaseTokenFact{"T2": {Revoked: true}}}
	got := releaseFacts{Tokens: map[string]releaseTokenFact{"T2": {Revoked: false}}}
	if releaseCompare(want, got) != "revocation-preserved" {
		t.Fatal("missing-re-revocation-not-detected")
	}
	got.Tokens["T2"] = want.Tokens["T2"]
	if releaseCompare(want, got) != "" {
		t.Fatal("re-revoked-state-rejected")
	}
}

type releaseFixture struct {
	traces                                     []releaseHTTPTrace
	observerID                                 string
	t                                          *testing.T
	server                                     *upgradeServer
	browser                                    *upgradeBrowser
	version                                    string
	parents                                    map[string]string
	parentIDs                                  map[string]string
	observer, activeToken                      string
	active                                     *matrixClient
	clientBinary, helper, home, cwd, auth, sid string
	before                                     protocol.Session
	nodes                                      map[string]memory.GetResult
	attachment                                 string
	apiToken, revokedAPI                       string
}

func releaseNew(t *testing.T, v, binary, client string) *releaseFixture {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	matrixRequire(t, e == nil, "release-loopback")
	address := listener.Addr().String()
	listener.Close()
	jar, _ := cookiejar.New(nil)
	server := &upgradeServer{t: t, address: address, dir: t.TempDir()}
	t.Cleanup(server.stop)
	f := &releaseFixture{t: t, server: server, version: v, clientBinary: client, parents: map[string]string{}, parentIDs: map[string]string{}, nodes: map[string]memory.GetResult{}}
	f.browser = &upgradeBrowser{t: t, origin: "http://" + address, client: &http.Client{Jar: jar, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	f.browser.observe = f.observeHTTP
	t.Cleanup(f.failureHTTP)
	server.start(binary)
	return f
}
func (f *releaseFixture) rest(method, path, bearer string, body []byte, content string) (int, []byte) {
	req, e := http.NewRequest(method, f.browser.origin+path, bytes.NewReader(body))
	matrixRequire(f.t, e == nil, "release-request")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if content != "" {
		req.Header.Set("Content-Type", content)
	}
	if content == "application/octet-stream" {
		req.Header.Set("X-XBroker-Attachment-Name", "synthetic.txt")
	}
	response, e := f.browser.client.Do(req)
	if e != nil {
		f.observeHTTP(method, path, 0, nil)
	}
	matrixRequire(f.t, e == nil, "release-http")
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	matrixRequire(f.t, e == nil, "release-response")
	f.observeHTTP(method, path, response.StatusCode, raw)
	return response.StatusCode, raw
}
func (f *releaseFixture) form(path string, fields url.Values) (int, []byte) {
	return f.rest("POST", path, "", []byte(fields.Encode()), "application/x-www-form-urlencoded")
}
func (f *releaseFixture) policy(path, field string) {
	page := f.browser.get("/admin/settings")
	form := regexp.MustCompile(`(?s)<form[^>]*action="` + regexp.QuoteMeta(path) + `"[^>]*>(.*?)</form>`).FindStringSubmatch(page)
	matrixRequire(f.t, len(form) == 2, "release-policy-form")
	guard := regexp.MustCompile(`name="settings_version" value="([^"]+)"`).FindStringSubmatch(form[1])
	matrixRequire(f.t, len(guard) == 2, "release-policy-guard")
	f.post(path, "/admin/settings", url.Values{field: {"true"}, "settings_version": {html.UnescapeString(guard[1])}})
}
func (f *releaseFixture) issue(label string, auto bool) string {
	return f.issueScopes(label, auto, access.LegacyV1Scopes())
}
func (f *releaseFixture) issueScopes(label string, auto bool, scopes []string) string {
	fields := url.Values{"project": {"test"}, "label": {label}, "scope_selection": {"custom"}, "scope": scopes}
	if auto {
		fields["scope"] = append(slices.Clone(fields["scope"]), access.SessionRegisterAuto)
		fields.Set("auto_cap", "unlimited")
		fields.Set("auto_days", "unlimited")
		if label == "capped" {
			fields.Set("auto_cap", "2")
			fields.Set("auto_days", "3")
		}
	}
	location := f.post("/tokens/project", "/projects/settings?project=test", fields)
	page := f.browser.get(location)
	match := regexp.MustCompile(`<code id="issued-token">([^<]+)</code>`).FindStringSubmatch(page)
	matrixRequire(f.t, len(match) == 2, "release-token-receipt")
	raw := html.UnescapeString(match[1])
	f.parents[label] = raw
	p, _ := autoClientEvidence(f.t, f.server.dir)
	for _, token := range p.Tokens {
		if token.Label == label {
			f.parentIDs[label] = token.ID
		}
	}
	matrixRequire(f.t, f.parentIDs[label] != "", "release-token-id")
	return raw
}
func (f *releaseFixture) revoke(label string) {
	f.post("/tokens/revoke", "/projects/settings?project=test", url.Values{"project": {"test"}, "id": {f.parentIDs[label]}})
}
func (f *releaseFixture) peer() *matrixClient {
	return releaseHTTPPeer(f.t, f.browser.origin, f.observer, f.version)
}
func (f *releaseFixture) seed() {
	t := f.t
	fields := url.Values{"password": {"synthetic-upgrade-password"}, "confirmPassword": {"synthetic-upgrade-password"}}
	if raw, err := os.ReadFile(filepath.Join(f.server.dir, "control", "setup-code")); err == nil {
		fields.Set("setupCode", strings.TrimSpace(string(raw)))
	}
	f.post("/setup", "/setup", fields)
	f.browser.login()
	f.post("/projects/create", "/", url.Values{"name": {"test"}})
	f.post("/projects/create", "/", url.Values{"name": {"second"}})
	f.policy("/admin/http-policy", "allowHTTP")
	f.policy("/admin/scope-issuance", "enabled")
	for _, name := range []string{"normal", "disabled"} {
		f.post("/admin/users/create", "/admin/users", url.Values{"username": {name}, "password": {"synthetic-upgrade-password"}, "role": {"user"}})
	}

	for _, row := range []struct{ project, user, role string }{{"test", "normal", "developer"}, {"second", "normal", "viewer"}, {"test", "disabled", "viewer"}} {
		f.post("/projects/members", "/projects/settings?project="+row.project, url.Values{"project": {row.project}, "username": {row.user}, "role": {row.role}, "action": {"add"}})
	}
	page := f.browser.get("/admin/users/detail?id=disabled")
	guard := regexp.MustCompile(`name="settings_version" value="([^"]+)"`).FindStringSubmatch(page)
	matrixRequire(t, len(guard) == 2, "release-user-guard")
	f.post("/admin/users/update", "/admin/users/detail?id=disabled", url.Values{"id": {"disabled"}, "role": {"user"}, "settings_version": {html.UnescapeString(guard[1])}})
	f.issueScopes("manual", false, f.seedScopes())
	f.issueScopes("T2", false, f.seedScopes())
	f.issueScopes("revoked", false, f.seedScopes())
	f.revoke("revoked")
	f.issueScopes("capped", true, f.seedScopes())
	f.issueScopes("unlimited", true, f.seedScopes())
	f.observer = f.browser.grant(f.parents["manual"], matrixPeerSID, "release-observer", "release-observer")
	project, _ := autoClientEvidence(t, f.server.dir)
	for _, token := range project.Tokens {
		if token.Nickname == "release-observer" {
			f.observerID = token.ID
		}
	}
	matrixRequire(t, f.observerID != "", "release-observer-id")
	peer := f.peer()
	var manager protocol.RegisterResult
	matrixRequire(t, peer.tool(t.Context(), "register_session", protocol.RegisterRequest{Harness: "codex", SessionID: matrixPeerSID, ClientID: "release-observer", PID: os.Getpid(), CWD: f.server.dir, ChannelID: "release-manager", Secret: clientstate.NewSecret(), RequestID: "release-manager"}, &manager) && manager.Code == "", "release-manager-registration")
	for i, name := range []string{"detached", "suspended", "auto-seed"} {
		sid := fmt.Sprintf("11111111-1111-4111-8111-%012d", 8100+i)
		clientID := fmt.Sprintf("release-seed-%d", i)
		token := ""
		if name == "auto-seed" {
			status, grant, _ := f.direct(f.parents["unlimited"], 8102)
			matrixRequire(t, status == 200, "release-auto-seed")
			token = grant.AccessToken
			sid = "22222222-2222-4222-8222-000000008102"
			clientID = "release-auto-8102"
		} else {
			token = f.browser.grant(f.parents["manual"], sid, clientID, name)
		}
		p := releaseHTTPPeer(t, f.browser.origin, token, f.version)
		var registered protocol.RegisterResult
		matrixRequire(t, p.tool(t.Context(), "register_session", protocol.RegisterRequest{Harness: "codex", SessionID: sid, ClientID: clientID, PID: os.Getpid(), CWD: f.server.dir, ChannelID: "release-seed", Secret: clientstate.NewSecret(), RequestID: name}, &registered) && registered.Code == "", "release-registration-seed")
		if name != "auto-seed" {
			var result any
			tool := "stop_session"
			if name == "suspended" {
				tool = "suspend_session"
			}
			matrixRequire(t, peer.tool(t.Context(), tool, map[string]string{"name": registered.Session.Name}, &result), "release-registration-state-"+name)
		}
		p.close()
	}
	for _, input := range []memory.PutInput{{Title: "original", Body: "synthetic original", CreatedBy: "release-observer"}, {Title: "linked", Body: "synthetic linked", CreatedBy: "release-observer"}, {Title: "replacement", Body: "synthetic replacement", CreatedBy: "release-observer"}, {Title: "archived", Body: "synthetic archived", CreatedBy: "release-observer", Archived: true}} {
		if input.Title == "linked" {
			for id := range f.nodes {
				input.Links = []string{id}
				break
			}
		}
		if input.Title == "replacement" {
			for id, node := range f.nodes {
				if node.Node.Title == "original" {
					input.Supersedes = id
				}
			}
		}
		if input.Archived {
			for id, node := range f.nodes {
				if node.Node.Title == "linked" {
					input.Supersedes = id
				}
			}
		}
		var node memory.Node
		matrixRequire(t, peer.tool(t.Context(), "memory_put", input, &node), "release-memory-seed-"+input.Title)
		f.nodes[node.ID] = memory.GetResult{Node: node}
	}
	for id := range f.nodes {
		var got memory.GetResult
		matrixRequire(t, peer.tool(t.Context(), "memory_get", map[string]string{"id": id}, &got), "release-memory-before")
		f.nodes[id] = got
	}
	peer.close()
	f.prepareClient()
	f.startClient()
	if cmp, _ := protocol.CompareVersions(f.version, "0.19.0"); cmp >= 0 {
		f.seedModern()
	}
	f.active.close()
	f.active = nil
	seeded, _ := autoClientEvidence(t, f.server.dir)
	parents, children := 0, 0
	issuesSpawn := f.sourceIssuesSpawn()
	for _, token := range seeded.Tokens {
		if issuesSpawn {
			matrixRequire(t, slices.Contains(token.Scopes, "session:spawn"), "historical-seed-spawn-present")
		} else {
			matrixRequire(t, !slices.Contains(token.Scopes, "session:spawn"), "modern-seed-spawn-absent")
		}
		if token.ParentID == "" {
			parents++
		} else {
			children++
		}
	}
	matrixRequire(t, parents == 5 && children >= 5, "historical-parent-child-coverage")
}
func (f *releaseFixture) prepareClient() {
	t := f.t
	self, e := os.Executable()
	matrixRequire(t, e == nil, "release-helper-executable")
	raw, e := os.ReadFile(self)
	matrixRequire(t, e == nil, "release-helper-read")
	f.helper = filepath.Join(t.TempDir(), "codex")
	matrixRequire(t, os.WriteFile(f.helper, raw, 0700) == nil, "release-helper-create")
	f.home, f.cwd, f.auth, f.sid = t.TempDir(), t.TempDir(), t.TempDir(), matrixSID
	dir := filepath.Join(f.home, ".codex", "sessions", "2026", "11", "07")
	matrixRequire(t, os.MkdirAll(dir, 0700) == nil, "release-rollout-dir")
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": f.sid, "cwd": f.cwd}})
	matrixRequire(t, os.WriteFile(filepath.Join(dir, "rollout-synthetic-"+f.sid+".jsonl"), append(meta, '\n'), 0600) == nil, "release-rollout-fixture")
	store, e := clientstate.Open(f.auth)
	matrixRequire(t, e == nil, "release-client-state")
	id, e := store.ClientID()
	matrixRequire(t, e == nil, "release-client-id")
	f.activeToken = f.browser.grant(f.parents["manual"], f.sid, id, "release-active")
}
func (f *releaseFixture) startClient() {
	f.active = matrixStart(f.t, f.clientBinary, f.helper, f.browser.origin+"/mcp?project=test", f.activeToken, f.auth, f.home, f.cwd, f.sid)
	if f.before.RegistrationID == "" {
		matrixRequire(f.t, f.active.tool(f.t.Context(), "attach_session", map[string]any{}, nil), "release-initial-explicit-attach")
	}
	matrixWait(f.t, "release-client-registration", func() bool {
		var status struct{ Registered bool }
		return f.active.tool(f.t.Context(), "auth_status", map[string]any{}, &status) && status.Registered
	})
	var got protocol.Session
	matrixRequire(f.t, f.active.tool(f.t.Context(), "get_session", map[string]string{"name": "release-active"}, &got), "release-client-observe")
	if f.before.RegistrationID != "" {
		matrixRequire(f.t, got.RegistrationID == f.before.RegistrationID && got.Epoch != f.before.Epoch, "release-same-registration-new-lease")
	}
	f.before = got
}

// Modern-only fixtures and assertions are below; every secret stays in memory.
func (f *releaseFixture) seedModern() {
	f.policy("/admin/admin-api", "enabled")
	// attachment_setting.go:142 accepts attachments (also in published 0.19.2).
	f.policy("/admin/attachments", "attachments")
	for _, label := range []string{"api-active", "api-revoked"} {
		body, _ := json.Marshal(map[string]any{"username": "admin", "password": "synthetic-upgrade-password", "label": label, "scopes": []string{"read", "write", "admin"}})
		status, raw := f.rest("POST", "/api/v1/login", "", body, "application/json")
		matrixRequire(f.t, status == 201, "release-api-seed")
		var token struct{ ID, Token string }
		matrixRequire(f.t, json.Unmarshal(raw, &token) == nil && token.Token != "", "release-api-grant")
		if label == "api-active" {
			f.apiToken = token.Token
		} else {
			f.revokedAPI = token.Token
			status, _ = f.rest("DELETE", "/api/v1/me/tokens/"+token.ID, f.apiToken, nil, "")
			matrixRequire(f.t, status == 204, "release-api-revoke")
		}
	}
	senderRegistered := func() {
		var status struct {
			Registered     bool
			RegistrationID string
		}
		matrixRequire(f.t, f.active != nil && f.active.tool(f.t.Context(), "auth_status", map[string]any{}, &status) && status.Registered && status.RegistrationID == f.before.RegistrationID, "attachment-sender-stays-registered")
	}
	senderRegistered()
	status, raw := f.rest("POST", "/attachments?project=test&from=release-active", f.activeToken, []byte("synthetic-pending-attachment"), "application/octet-stream")
	matrixRequire(f.t, status == 201, "release-attachment-seed")
	var ref protocol.Attachment
	matrixRequire(f.t, json.Unmarshal(raw, &ref) == nil && ref.ID != "", "release-attachment-reference")
	f.attachment = ref.ID
	senderRegistered()
}
func (f *releaseFixture) direct(parent string, index int) (int, controlpanel.DirectResponse, string) {
	fields := url.Values{"client_id": {"asb-client-v1"}, "harness": {"codex"}, "session_id": {fmt.Sprintf("22222222-2222-4222-8222-%012d", index)}, "asb_client_instance_id": {fmt.Sprintf("release-auto-%d", index)}, "request_id": {fmt.Sprintf("release-auto-%d", index)}, "requested_nickname": {fmt.Sprintf("release-auto-%d", index)}}
	status, raw := f.rest("POST", "/oauth/session_registration?project=test", parent, []byte(fields.Encode()), "application/x-www-form-urlencoded")
	var grant controlpanel.DirectResponse
	var failure struct{ Error string }
	if status == 200 {
		matrixRequire(f.t, json.Unmarshal(raw, &grant) == nil, "release-direct-decode")
	} else {
		matrixRequire(f.t, json.Unmarshal(raw, &failure) == nil, "release-direct-refusal")
	}
	return status, grant, failure.Error
}
func (f *releaseFixture) quota() {
	t := f.t
	parent, _ := autoClientEvidence(t, f.server.dir)
	tokens := map[string]controlpanel.SessionToken{}
	for _, token := range parent.Tokens {
		tokens[token.Label] = token
	}
	capParent, freeParent := tokens["capped"], tokens["unlimited"]
	matrixRequire(t, capParent.ExpiresAt.Sub(capParent.IssuedAt) == 3*24*time.Hour && freeParent.ExpiresAt.IsZero(), "parent-lifetime-preserved")
	for _, label := range []string{"capped", "unlimited"} {
		n := 2
		if label == "unlimited" {
			n = 9
		}
		for i := 0; i < n; i++ {
			index := 9000 + i
			if label == "unlimited" {
				index += 100
			}
			started := time.Now()
			status, grant, _ := f.direct(f.parents[label], index)
			matrixRequire(t, status == 200 && grant.AccessToken != "", "auto-limit-admission")
			peer := releaseHTTPPeer(t, f.browser.origin, grant.AccessToken, f.version)
			var registered protocol.RegisterResult
			matrixRequire(t, peer.tool(t.Context(), "register_session", protocol.RegisterRequest{Harness: "codex", SessionID: fmt.Sprintf("22222222-2222-4222-8222-%012d", index), ClientID: fmt.Sprintf("release-auto-%d", index), PID: os.Getpid(), CWD: f.server.dir, ChannelID: "quota", Secret: clientstate.NewSecret(), RequestID: fmt.Sprintf("quota-%d", index)}, &registered) && registered.Code == "", "auto-registration-admitted")
			var info access.TokenInfo
			matrixRequire(t, peer.tool(t.Context(), access.TokenInfoTool, map[string]any{}, &info), "auto-lifetime-info")
			expected := started.Add(30 * 24 * time.Hour)
			if label == "capped" {
				expected = capParent.ExpiresAt
			}
			matrixRequire(t, !info.ExpiresAt.IsZero() && info.ExpiresAt.Sub(expected).Abs() < time.Minute, "child-lifetime-preserved")
			peer.close()
		}
		if label == "capped" {
			status, _, code := f.direct(f.parents[label], 9999)
			matrixRequire(t, status == 409 && code == "capacity_exceeded", "auto-capacity-refused")
		}
	}
}
func (f *releaseFixture) budgetProbe() {
	t := f.t
	project, _ := autoClientEvidence(t, f.server.dir)
	active := 0
	for _, token := range project.Tokens {
		if !token.Revoked && (token.ExpiresAt.IsZero() || token.ExpiresAt.After(time.Now())) {
			active++
		}
	}
	// source/internal/controlpanel/token_cap.go:9: projectTokenCap is 256 (also in 0.17.3).
	for i := active; i < 256; i++ {
		f.issue(fmt.Sprintf("capacity-%d", i), false)
	}
	page := f.browser.get("/projects/settings?project=test")
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page)
	matrixRequire(t, len(csrf) == 2, "capacity-csrf")
	status, _ := f.form("/tokens/project", url.Values{"project": {"test"}, "label": {"over-capacity"}, "csrf": {html.UnescapeString(csrf[1])}})
	matrixRequire(t, status == 400, "project-token-capacity-enforced")
}

func (f *releaseFixture) assertInvariant(want releaseFacts, candidate bool) {
	t := f.t
	f.browser.login()
	got := releaseReadFacts(t, f.server.dir)
	if candidate {
		want = releaseMigratedFacts(want)
	}
	if invariant := releaseCompare(want, got); invariant != "" {
		t.Fatal(invariant)
	}
	// Login failure and Viewer mutation are behavior, not just stored role bits.
	for _, row := range []struct {
		user   string
		status int
	}{{"normal", 303}, {"disabled", 401}} {
		jar, _ := cookiejar.New(nil)
		browser := &upgradeBrowser{t: t, origin: f.browser.origin, client: &http.Client{Jar: jar, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
		csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(browser.get("/login"))
		matrixRequire(t, len(csrf) == 2, "invariant-login-csrf")
		response, e := browser.client.PostForm(browser.origin+"/login", url.Values{"username": {row.user}, "password": {"synthetic-upgrade-password"}, "csrf": {html.UnescapeString(csrf[1])}})
		matrixRequire(t, e == nil, "invariant-login-http")
		response.Body.Close()
		matrixRequire(t, response.StatusCode == row.status, "user-login-preserved")
		if row.user == "normal" {
			csrf = regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(browser.get("/projects/settings?project=second"))
			matrixRequire(t, len(csrf) == 2, "viewer-csrf")
			response, e = browser.client.PostForm(browser.origin+"/projects/save", url.Values{"project": {"second"}, "mode": {"token"}, "enabled": {"true"}, "csrf": {html.UnescapeString(csrf[1])}})
			matrixRequire(t, e == nil, "viewer-http")
			response.Body.Close()
			matrixRequire(t, response.StatusCode == 403, "viewer-write-refused")
		}
	}
	for _, label := range []string{"revoked"} {
		status, _ := f.rest("POST", "/oauth/device_authorization?project=test", f.parents[label], []byte(url.Values{"client_id": {"asb-client-v1"}, "harness": {"codex"}, "session_id": {"33333333-3333-4333-8333-333333333333"}, "asb_client_instance_id": {"release-invariant"}, "request_id": {"release-invariant"}}.Encode()), "application/x-www-form-urlencoded")
		matrixRequire(t, status == 401, "revocation-preserved")
	}
	// The approved T2 expectation distinguishes restored S0 from re-revoked S0'.
	status, _ := f.rest("POST", "/oauth/device_authorization?project=test", f.parents["T2"], []byte(url.Values{"client_id": {"asb-client-v1"}, "harness": {"codex"}, "session_id": {"33333333-3333-4333-8333-333333333333"}, "asb_client_instance_id": {"release-invariant"}, "request_id": {"release-invariant"}}.Encode()), "application/x-www-form-urlencoded")
	if want.Tokens[f.parentIDs["T2"]].Revoked {
		matrixRequire(t, status == 401, "re-revocation-preserved")
	} else {
		matrixRequire(t, status == 200, "restored-revocation-caveat")
	}
	peer := f.peer()
	defer peer.close()
	var info access.TokenInfo
	matrixRequire(t, peer.tool(t.Context(), access.TokenInfoTool, map[string]any{}, &info), "session-scopes-available")
	expectedInfo := want.Tokens[f.observerID]
	matrixRequire(t, slices.Equal(info.Scopes, expectedInfo.Scopes) && info.AdmissionMode == expectedInfo.Mode && info.ExpiresAt.Equal(expectedInfo.Expires), "session-scopes-lifetime-preserved")
	for id, want := range f.nodes {
		var got memory.GetResult
		matrixRequire(t, peer.tool(t.Context(), "memory_get", map[string]string{"id": id}, &got) && reflect.DeepEqual(got, want), "memory-content-links-history")
	}
	for _, history := range []bool{false, true} {
		var result memory.SearchResult
		matrixRequire(t, peer.tool(t.Context(), "memory_search", memory.SearchInput{IncludeHistory: history}, &result), "memory-search")
		ids := map[string]bool{}
		for _, node := range result.Nodes {
			ids[node.ID] = true
		}
		expectedCount := 0
		for id, node := range f.nodes {
			hidden := node.Node.Archived || node.ReplacedBy != ""
			matrixRequire(t, ids[id] == (history || !hidden), "memory-history-filter")
			if history || !hidden {
				expectedCount++
			}
		}
		matrixRequire(t, len(ids) == expectedCount, "memory-no-extra-nodes")
	}
	// Configured data must never re-open setup or generate a new setup proof.
	status, _ = f.rest("GET", "/setup", "", nil, "")
	matrixRequire(t, status == 409, "setup-not-reopened")
	_, e := os.Stat(filepath.Join(f.server.dir, "control", "setup-code"))
	matrixRequire(t, os.IsNotExist(e), "setup-proof-absent")
	if f.apiToken != "" {
		status, _ = f.rest("GET", "/api/v1/me", f.apiToken, nil, "")
		matrixRequire(t, status == 200, "api-token-preserved")
		status, _ = f.rest("GET", "/api/v1/me", f.revokedAPI, nil, "")
		matrixRequire(t, status == 401, "api-revocation-preserved")
	}
	if f.attachment != "" {
		status, raw := f.rest("GET", "/attachments/"+f.attachment+"?project=test&recipient=release-active", f.activeToken, nil, "")
		var failure struct{ Code string }
		_ = json.Unmarshal(raw, &failure)
		matrixRequire(t, status == 404 && failure.Code == "unavailable", "attachment-startup-cleared")
		entries, e := os.ReadDir(filepath.Join(f.server.dir, "attachments-v1"))
		matrixRequire(t, e == nil && len(entries) == 0, "attachment-spool-empty")
	}
}

type releaseAssetManifest struct {
	Candidate string `json:"candidate"`
	Assets    []struct{ Version, Kind, Platform, Path, SHA256 string }
}
type releaseLock struct {
	Candidate, Previous string
	UpgradeSources      []string
	Assets              []struct{ Version, Kind, Platform, SHA256 string }
}

func releaseInputs(t *testing.T, candidate bool) (releaseLock, releaseAssetManifest, string) {
	t.Helper()
	var lock releaseLock
	raw, e := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "release", "compat-assets.json"))
	matrixRequire(t, e == nil && json.Unmarshal(raw, &lock) == nil, "release-lock-required")
	var manifest releaseAssetManifest
	raw, e = os.ReadFile(os.Getenv("ASB_RC_ASSET_MANIFEST"))
	matrixRequire(t, e == nil && json.Unmarshal(raw, &manifest) == nil && manifest.Candidate == lock.Candidate, "release-manifest-required")
	for _, row := range lock.Assets {
		if row.Kind == "image" {
			continue
		}
		found := 0
		for _, asset := range manifest.Assets {
			if asset.Kind == row.Kind && asset.Version == row.Version && asset.Platform == row.Platform {
				matrixRequire(t, asset.SHA256 == row.SHA256, "release-lock-hash")
				found++
			}
		}
		matrixRequire(t, found == 1, "release-required-asset")
	}
	binary := ""
	if candidate {
		server, ok := protocol.ServerPolicy(lock.Candidate)
		matrixRequire(t, ok && server.Line == protocol.VersionLine(lock.Candidate) && lock.Candidate != "0.0.0" && lock.Candidate != "dev", "release-exact-server-row")
		client, ok := protocol.ClientPolicy(lock.Candidate)
		matrixRequire(t, ok && client.Line == protocol.VersionLine(lock.Candidate), "release-exact-client-row")
		sources := []string{server.MinClient, lock.Previous}
		sort.Strings(sources)
		sources = slices.Compact(sources)
		matrixRequire(t, slices.Equal(sources, lock.UpgradeSources), "release-required-upgrade-sources")
		binary = os.Getenv("ASB_RC_CANDIDATE_SERVER")
		var evidence struct {
			Candidate string
			SHA256    map[string]string
		}
		raw, e = os.ReadFile(os.Getenv("ASB_RC_BUILD_EVIDENCE"))
		matrixRequire(t, e == nil && json.Unmarshal(raw, &evidence) == nil && evidence.Candidate == lock.Candidate, "release-build-evidence-required")
		releaseVerifyBinary(t, binary, evidence.SHA256["server"], "server", lock.Candidate)
	}
	return lock, manifest, binary
}
func releaseVerifyBinary(t *testing.T, path, digest, kind, version string) {
	info, e := os.Lstat(path)
	matrixRequire(t, e == nil && info.Mode().IsRegular(), "release-regular-binary")
	raw, e := os.ReadFile(path)
	sum := sha256.Sum256(raw)
	matrixRequire(t, e == nil && digest != "" && hex.EncodeToString(sum[:]) == digest, "release-binary-sha256")
	matrixRequire(t, os.Chmod(path, 0700) == nil, "release-binary-chmod")
	out, e := exec.Command(path, "--version").Output()
	matrixRequire(t, e == nil && strings.TrimSpace(string(out)) == "asb-"+kind+" "+version, "release-binary-version")
}
func releaseBinary(t *testing.T, m releaseAssetManifest, kind, version string) string {
	for _, row := range m.Assets {
		if row.Kind == kind && row.Version == version && row.Platform == "darwin-arm64" {
			releaseVerifyBinary(t, row.Path, row.SHA256, kind, version)
			return row.Path
		}
	}
	t.Fatal("release-binary-required")
	return ""
}
func releaseGate(t *testing.T) {
	if os.Getenv("ASB_RC_GATE") != "1" {
		t.Skip("opt-in: release candidate upgrade gate")
	}
	matrixRequire(t, runtime.GOOS == "darwin" && runtime.GOARCH == "arm64", "release-gate-host")
}
func (f *releaseFixture) stage(binary string, want releaseFacts, candidate bool) {
	f.server.stop()
	clone := filepath.Join(f.t.TempDir(), "quota-data")
	manifest, e := releaseBackup(f.server, clone)
	matrixRequire(f.t, e == nil, "quota-stopped-manifest")
	got, e := releaseTree(clone)
	matrixRequire(f.t, e == nil && reflect.DeepEqual(got, manifest), "quota-clone-integrity")
	f.server.start(binary)
	// A restored registration is retired until the saved client resumes its lease.
	f.startClient()
	f.assertInvariant(want, candidate)
	f.active.close()
	f.active = nil
	f.server.stop()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	matrixRequire(f.t, e == nil, "quota-loopback")
	address := listener.Addr().String()
	listener.Close()
	probe := *f
	probe.parents = maps.Clone(f.parents)
	probe.parentIDs = maps.Clone(f.parentIDs)
	probe.server = &upgradeServer{t: f.t, address: address, dir: clone}
	jar, _ := cookiejar.New(nil)
	probe.browser = &upgradeBrowser{t: f.t, origin: "http://" + address, client: &http.Client{Jar: jar, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	probe.traces = nil
	probe.browser.observe = probe.observeHTTP
	f.t.Cleanup(probe.failureHTTP)
	rateClone := filepath.Join(f.t.TempDir(), "rate-data")
	matrixRequire(f.t, releaseRestore(clone, rateClone, manifest) == nil, "rate-clone-integrity")
	probe.server.dir = rateClone
	probe.server.start(binary)
	defer probe.server.stop()
	probe.browser.login()
	probe.rateProbe()
	probe.server.stop()
	probe.server.dir = clone
	probe.server.start(binary)
	probe.quota()
	probe.browser.login()
	probe.budgetProbe()
}
func releaseEvidence(t *testing.T, kind string, expected, passed []string) {
	t.Helper()
	sort.Strings(expected)
	sort.Strings(passed)
	matrixRequire(t, len(expected) > 0 && slices.Equal(expected, passed), "release-exact-case-set")
	if dir := os.Getenv("ASB_RC_EVIDENCE_DIR"); dir != "" {
		matrixRequire(t, os.MkdirAll(dir, 0700) == nil, "release-evidence-dir")
		raw, _ := json.Marshal(map[string]any{"schema": 1, "expected": expected, "passed": passed})
		matrixRequire(t, os.WriteFile(filepath.Join(dir, kind+"-evidence.json"), raw, 0600) == nil, "release-evidence-write")
	}
}
func TestReleaseUpgradeInvariants(t *testing.T) {
	releaseGate(t)
	lock, manifest, candidate := releaseInputs(t, true)
	expected, passed := []string{}, []string{}
	for _, version := range lock.UpgradeSources {
		expected = append(expected, "upgrade/"+version)
		if !t.Run(version, func(t *testing.T) {
			f := releaseNew(t, version, releaseBinary(t, manifest, "server", version), releaseBinary(t, manifest, "client", version))
			f.seed()
			want := releaseReadFacts(t, f.server.dir)
			f.stage(candidate, want, true)
		}) {
			return
		}
		passed = append(passed, "upgrade/"+version)
	}
	releaseEvidence(t, "upgrade", expected, passed)
}
func TestReleaseRestoreRehearsal(t *testing.T) {
	releaseGate(t)
	lock, manifest, candidate := releaseInputs(t, true)
	old := releaseBinary(t, manifest, "server", lock.Previous)
	f := releaseNew(t, lock.Previous, old, releaseBinary(t, manifest, "client", lock.Previous))
	f.seed()
	s0 := releaseReadFacts(t, f.server.dir)
	f.server.stop()
	backup := filepath.Join(t.TempDir(), "backup")
	receipt, e := releaseBackup(f.server, backup)
	matrixRequire(t, e == nil, "restore-stopped-backup")
	// C1: candidate changes are deliberately outside the stopped backup.
	f.stage(candidate, s0, true)
	f.server.start(candidate)
	f.browser.login()
	f.revoke("T2")
	t3 := f.issue("T3", false)
	peer := f.peer()
	var m2 memory.Node
	matrixRequire(t, peer.tool(t.Context(), "memory_put", memory.PutInput{Title: "post-backup", Body: "synthetic M2", CreatedBy: "release-observer"}, &m2), "restore-post-backup-memory")
	peer.close()
	f.server.stop()
	// C2: a NEW empty destination, checked byte-for-byte before the old server starts.
	restored := filepath.Join(t.TempDir(), "restored")
	matrixRequire(t, releaseRestore(backup, restored, receipt) == nil, "restore-manifest-before-start")
	f.server.dir = restored
	f.stage(old, s0, false)
	f.server.start(old)
	f.browser.login()
	status, _ := f.rest("POST", "/oauth/device_authorization?project=test", t3, []byte(url.Values{"client_id": {"asb-client-v1"}, "harness": {"codex"}, "session_id": {"33333333-3333-4333-8333-333333333333"}, "asb_client_instance_id": {"release-invariant"}, "request_id": {"release-invariant"}}.Encode()), "application/x-www-form-urlencoded")
	matrixRequire(t, status == 401, "restore-post-backup-token-absent")
	peer = f.peer()
	var absent memory.GetResult
	matrixRequire(t, !peer.tool(t.Context(), "memory_get", map[string]string{"id": m2.ID}, &absent), "restore-post-backup-memory-absent")
	peer.close()
	// C3 -> S0': omitting this revoke is caught by the C4 revocation invariant.
	f.revoke("T2")
	f.server.stop()
	s0.Tokens = cloneReleaseTokens(s0.Tokens)
	token := s0.Tokens[f.parentIDs["T2"]]
	token.Revoked = true
	s0.Tokens[f.parentIDs["T2"]] = token
	// C3 re-revokes from /projects/settings; its ordinary session list removes
	// detached rows (serverruntime/managed.go sessionRows cleanup), so S0' has none.
	before := len(s0.Registrations)
	s0.Registrations = maps.Clone(s0.Registrations)
	maps.DeleteFunc(s0.Registrations, func(_ string, r releaseRegistrationFact) bool { return r.Detached })
	matrixRequire(t, before-len(s0.Registrations) == 1, "restore-detached-cleanup-modelled")
	// C4 is I(S0'), never I(S0), and never an in-place downgrade.
	f.stage(candidate, s0, true)
	releaseEvidence(t, "restore", []string{"restore/" + lock.Previous}, []string{"restore/" + lock.Previous})
}
func cloneReleaseTokens(tokens map[string]releaseTokenFact) map[string]releaseTokenFact {
	out := map[string]releaseTokenFact{}
	for key, value := range tokens {
		out[key] = value
	}
	return out
}
func TestReleaseOldServerSeedAndStoppedRestore(t *testing.T) {
	if os.Getenv("ASB_RC_ASSET_MANIFEST") == "" {
		t.Skip("opt-in: real old server seed verification")
	}
	matrixRequire(t, runtime.GOOS == "darwin" && runtime.GOARCH == "arm64", "release-seed-host")
	lock, manifest, _ := releaseInputs(t, false)
	for _, version := range lock.UpgradeSources {
		if !t.Run(version, func(t *testing.T) {
			old := releaseBinary(t, manifest, "server", version)
			f := releaseNew(t, version, old, releaseBinary(t, manifest, "client", version))
			f.seed()
			before := releaseReadFacts(t, f.server.dir)
			f.server.stop()
			backup := filepath.Join(t.TempDir(), "backup")
			receipt, e := releaseBackup(f.server, backup)
			matrixRequire(t, e == nil, "seed-stopped-backup")
			restored := filepath.Join(t.TempDir(), "restore")
			matrixRequire(t, releaseRestore(backup, restored, receipt) == nil, "seed-restore-bytes")
			f.server.dir = restored
			f.stage(old, before, false)
		}) {
			return
		}
	}
}

func (f *releaseFixture) post(path, page string, fields url.Values) string {
	steps := map[string]string{"/setup": "setup", "/projects/create": "project-create", "/projects/members": "member-add", "/admin/users/create": "user-create", "/admin/users/update": "user-update", "/admin/http-policy": "http-policy", "/admin/scope-issuance": "scope-policy", "/tokens/project": "token-issue", "/tokens/revoke": "token-revoke", "/admin/admin-api": "api-policy", "/admin/attachments": "attachment-policy"}
	step := steps[path]
	if step == "" {
		step = "other"
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(f.browser.get(page))
	matrixRequire(f.t, len(csrf) == 2, "release-form-csrf")
	fields.Set("csrf", html.UnescapeString(csrf[1]))
	response, e := f.browser.client.PostForm(f.browser.origin+path, fields)
	if e != nil {
		f.observeHTTP("POST", path, 0, nil)
	}
	matrixRequire(f.t, e == nil, "release-form-http")
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	f.observeHTTP("POST", path, response.StatusCode, raw)
	if response.StatusCode != 303 {
		f.t.Fatalf("release-form-%s status=%d", step, response.StatusCode)
	}
	return response.Header.Get("Location")
}

type releaseHTTPTrace struct {
	Step   string
	Status int
	Code   string
}

func releaseHTTPCode(raw []byte) string {
	var value struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return "none"
	}
	code := value.Code
	if code == "" {
		code = value.Error
	}
	switch code {
	case "":
		return "none"
	case "invalid", "unavailable", "attachments-disabled", "server-disk-low", "server-disk-unknown", "invalid_request", "invalid_client", "invalid_token", "insufficient_scope", "capacity_exceeded", "rate_limited", "identity_conflict", "http_not_allowed", "forbidden", "generation_conflict", "server_error", "not_found", "api_disabled", "token_cap_reached", "busy", "too-large", "retired":
		return code
	default:
		return "other"
	}
}
func (f *releaseFixture) observeHTTP(method, path string, status int, raw []byte) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	step := "other"
	switch path {
	case "/setup":
		step = "setup"
	case "/login":
		step = "login"
	case "/projects/create":
		step = "project-create"
	case "/projects/members":
		step = "member"
	case "/admin/users/create":
		step = "user-create"
	case "/admin/users/update":
		step = "user-update"
	case "/admin/http-policy":
		step = "http-policy"
	case "/admin/scope-issuance":
		step = "scope-policy"
	case "/admin/attachments":
		step = "attachment-policy"
	case "/admin/admin-api":
		step = "api-policy"
	case "/tokens/project":
		step = "token-issue"
	case "/tokens/revoke":
		step = "token-revoke"
	case "/oauth/device_authorization":
		step = "device-request"
	case "/oauth/token":
		step = "device-token"
	case "/oauth/session_registration":
		step = "direct"
	case "/auth/code":
		step = "device-approve"
	case "/api/v1/login":
		step = "api-token-issue"
	case "/api/v1/me":
		step = "api-me"
	case "/attachments":
		step = "attachment-upload"
	default:
		if strings.HasPrefix(path, "/api/v1/me/tokens/") {
			step = "api-token-revoke"
		} else if strings.HasPrefix(path, "/attachments/") {
			step = "attachment-download"
		} else if method == "GET" {
			step = "page"
		}
	}
	code := releaseHTTPCode(raw)
	if status == 0 {
		code = "transport"
	}
	f.traces = append(f.traces, releaseHTTPTrace{step, status, code})
}
func (f *releaseFixture) failureHTTP() {
	if !f.t.Failed() {
		return
	}
	for _, trace := range f.traces {
		f.t.Logf("seed-http step=%s status=%d code=%s", trace.Step, trace.Status, trace.Code)
	}
}
func TestReleaseHTTPDiagnosticAllowlist(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{`{"code":"unavailable"}`, "unavailable"}, {`{"error":"invalid_token"}`, "invalid_token"}, {`{"code":"private-value-that-must-not-appear"}`, "other"}, {`<private page>`, "none"}} {
		if releaseHTTPCode([]byte(tc.raw)) != tc.want {
			t.Fatal("http-diagnostic-code-allowlist")
		}
	}
}

// Historical fixtures must not follow a candidate's changing issuance catalog.
func releaseHistoricalScopes() []string {
	// Frozen 0.17.3/0.19.2 contract, independent of candidate issuance changes.
	return []string{"memory:read", "memory:write", "message:receive", "message:send", "session:manage", "session:read", "session:register", "session:rename", "session:spawn"}
}

// releaseSourceIssuesSpawn reports whether a source version still issues
// session:spawn (0.20.0 removed it) and whether the version parsed at all.
func releaseSourceIssuesSpawn(version string) (issues, ok bool) {
	cmp, ok := protocol.CompareVersions(version, "0.20.0")
	return ok && cmp < 0, ok
}

func (f *releaseFixture) sourceIssuesSpawn() bool {
	issues, ok := releaseSourceIssuesSpawn(f.version)
	matrixRequire(f.t, ok, "release-source-version-parse")
	return issues
}

// seedScopes is what the source under test can really issue.
func (f *releaseFixture) seedScopes() []string {
	if f.sourceIssuesSpawn() {
		return releaseHistoricalScopes()
	}
	return access.LegacyV1Scopes()
}

func (f *releaseFixture) rateProbe() {
	parent := f.issue("rate-probe", true)
	index := 9300
	first, failure := releaseRateBoundary(func() (int, string) {
		index++
		status, _, code := f.direct(parent, index)
		return status, code
	}, time.Now, time.Sleep)
	matrixRequire(f.t, failure == "", failure)
	f.t.Logf("request-rate first-refused=%d refill=verified", first)
}

// Fresh unlimited parent: burst10, refill30/minute (direct_rate.go).
// Request windows bound the server decision time; ambiguity fails closed.
func releaseRateBoundary(call func() (int, string), now func() time.Time, sleep func(time.Duration)) (int, string) {
	lower, upper := 10.0, 10.0
	lastStart, lastEnd := now(), now()
	request := func() (bool, string) {
		start := now()
		status, code := call()
		end := now()
		lower = min(10, lower+float64(start.Sub(lastEnd))/float64(2*time.Second))
		upper = min(10, upper+float64(end.Sub(lastStart))/float64(2*time.Second))
		lastStart, lastEnd = start, end
		if lower >= 1 {
			if status != 200 {
				return false, "request-rate-allowed-boundary"
			}
			lower--
			upper--
			return false, ""
		}
		if upper >= 1 {
			return false, "request-rate-timing-ambiguous"
		}
		if status != 429 || code != "rate_limited" {
			return false, "request-rate-refused-boundary"
		}
		return true, ""
	}
	first := 0
	for i := 1; i <= 20; i++ {
		refused, failure := request()
		if failure != "" {
			return 0, failure
		}
		if refused {
			first = i
			break
		}
	}
	if first == 0 {
		return 0, "request-rate-no-boundary"
	}
	sleep(2 * time.Second)
	refilled := 0
	for i := 0; i < 20; i++ {
		refused, failure := request()
		if failure != "" {
			return first, failure
		}
		if refused {
			if refilled == 0 {
				return first, "request-rate-refill-missing"
			}
			return first, ""
		}
		refilled++
	}
	return first, "request-rate-refill-unbounded"
}

func TestReleaseHistoricalScopesFrozen(t *testing.T) {
	if scopes := releaseHistoricalScopes(); len(scopes) != 9 || !slices.Contains(scopes, "session:spawn") {
		t.Fatal("historical-scopes-not-frozen")
	}
}

func TestReleaseSourceIssuesSpawnBoundary(t *testing.T) {
	for _, row := range []struct {
		version    string
		issues, ok bool
	}{{"0.17.3", true, true}, {"0.19.3", true, true}, {"0.20.0", false, true}, {"0.20.1", false, true}, {"1.0.0", false, true}, {"not-a-version", false, false}, {"", false, false}} {
		if issues, ok := releaseSourceIssuesSpawn(row.version); issues != row.issues || ok != row.ok {
			t.Fatalf("%q: issues=%v ok=%v", row.version, issues, ok)
		}
	}
}

func TestReleaseRateBoundaryRejectsMutants(t *testing.T) {
	for _, row := range []struct {
		name   string
		burst  int
		spent  int
		refill time.Duration
		valid  bool
	}{
		{"contract", 10, 0, 2 * time.Second, true}, {"burst20", 20, 0, 2 * time.Second, false}, {"burst20-after-quota", 20, 9, 2 * time.Second, false}, {"double-refill", 10, 0, time.Second, false}, {"half-refill", 10, 0, 4 * time.Second, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			clock := time.Unix(100, 0)
			at := clock
			balance := float64(row.burst - row.spent)
			call := func() (int, string) {
				balance = min(float64(row.burst), balance+float64(clock.Sub(at))/float64(row.refill))
				at = clock
				if balance < 1 {
					return 429, "rate_limited"
				}
				balance--
				return 200, ""
			}
			first, failure := releaseRateBoundary(call, func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) })
			if row.valid && (failure != "" || first != 11) {
				t.Fatal("rate-contract-rejected")
			}
			if !row.valid && failure == "" {
				t.Fatal("rate-mutation-not-detected")
			}
		})
	}
}

func releaseMigratedFacts(want releaseFacts) releaseFacts {
	want.Tokens = cloneReleaseTokens(want.Tokens)
	for id, token := range want.Tokens {
		token.Scopes = slices.DeleteFunc(slices.Clone(token.Scopes), func(scope string) bool { return scope == "session:spawn" })
		want.Tokens[id] = token
	}
	return want
}

func TestReleaseSpawnStripIsRequired(t *testing.T) {
	old := releaseFacts{Tokens: map[string]releaseTokenFact{"historical": {Scopes: releaseHistoricalScopes()}}}
	want := releaseMigratedFacts(old)
	if releaseCompare(want, old) != "scopes-lifetime-preserved" {
		t.Fatal("no-op-spawn-migration-not-detected")
	}
	got := releaseMigratedFacts(old)
	if releaseCompare(want, got) != "" {
		t.Fatal("spawn-strip-rejected")
	}
	if !slices.Contains(old.Tokens["historical"].Scopes, "session:spawn") {
		t.Fatal("historical-facts-mutated")
	}
}
