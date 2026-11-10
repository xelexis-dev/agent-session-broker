package serverruntime

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func releaseHTTPPeer(t *testing.T, origin, token, version string) *matrixClient {
	t.Helper()
	client := &http.Client{Transport: matrixBearerTransport{token: token}}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "agent-session-broker-client", Version: version}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: origin + "/mcp?project=test", HTTPClient: client}, nil)
	matrixRequire(t, err == nil, "release-peer-initialize")
	c := &matrixClient{session}
	t.Cleanup(c.close)
	return c
}

func releaseCompatibilityCases() []string {
	return []string{"client/previous", "client/oldest", "client/below-minimum", "client/placeholder", "server/previous"}
}

func (f *releaseFixture) compatibilitySetup() {
	fields := url.Values{"password": {"synthetic-upgrade-password"}, "confirmPassword": {"synthetic-upgrade-password"}}
	if raw, err := os.ReadFile(filepath.Join(f.server.dir, "control", "setup-code")); err == nil {
		fields.Set("setupCode", strings.TrimSpace(string(raw)))
	}
	f.post("/setup", "/setup", fields)
	f.browser.login()
	f.post("/projects/create", "/", url.Values{"name": {"test"}})
	f.policy("/admin/http-policy", "allowHTTP")
	f.policy("/admin/scope-issuance", "enabled")
	f.issue("manual", false)
	f.issue("auto", true)
	f.observer = f.browser.grant(f.parents["manual"], matrixPeerSID, "release-observer", "release-observer")
}

func (f *releaseFixture) compatibilityAllowed(version, mode string) {
	t := f.t
	f.prepareClient()
	token := f.activeToken
	if mode == "auto" {
		token = f.parents["auto"]
	}
	c := matrixStart(t, f.clientBinary, f.helper, f.browser.origin+"/mcp?project=test", token, f.auth, f.home, f.cwd, f.sid)
	defer c.close()
	matrixRequire(t, c.tool(t.Context(), "attach_session", map[string]any{}, nil), "release-compatible-attach")
	var status struct {
		Registered, Compatible bool
		Name                   string
	}
	matrixWait(t, "release-compatible-authorized", func() bool { return c.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Registered })
	matrixRequire(t, status.Compatible && status.Name != "", "release-compatible-status")
	listed, e := c.session.ListTools(t.Context(), nil)
	matrixRequire(t, e == nil && len(listed.Tools) > 0, "release-compatible-tool-list")
	matrixRequire(t, c.tool(t.Context(), "ingress_hook", map[string]string{"session_id": f.sid, "hook_event_name": "PreToolUse"}, nil), "release-compatible-ingress")
	peer := releaseHTTPPeer(t, f.browser.origin, f.observer, version)
	defer peer.close()
	var registered protocol.RegisterResult
	matrixRequire(t, peer.tool(t.Context(), "register_session", protocol.RegisterRequest{Harness: "codex", SessionID: matrixPeerSID, ClientID: "release-observer", PID: os.Getpid(), CWD: f.server.dir, ChannelID: "release-observer", Secret: clientstate.NewSecret(), RequestID: "release-observer"}, &registered) && registered.Code == "", "release-compatible-peer")
	body := "rc-compat-client-to-server"
	var sent protocol.SendResult
	matrixRequire(t, c.tool(t.Context(), "send_message", protocol.Message{From: status.Name, To: registered.Session.Name, Body: body}, &sent) && sent.Accepted, "release-compatible-send-to-peer")
	var got protocol.Message
	matrixRequire(t, peer.tool(t.Context(), "wait_for_message", map[string]string{"recipient": registered.Session.Name, "channelId": "release-observer"}, &got) && got.Body == body, "release-compatible-peer-received")
	body = "rc-compat-server-to-client"
	matrixRequire(t, peer.tool(t.Context(), "send_message", protocol.Message{From: registered.Session.Name, To: status.Name, Body: body}, &sent) && sent.Accepted, "release-compatible-send-to-client")
	matrixWait(t, "release-compatible-client-received", func() bool {
		var hook map[string]any
		if !c.tool(t.Context(), "ingress_hook", map[string]string{"session_id": f.sid, "hook_event_name": "PostToolUse"}, &hook) {
			return false
		}
		raw, _ := json.Marshal(hook)
		return strings.Contains(string(raw), body)
	})
}

func releaseRefusalInventory(listed []*mcp.Tool) []string {
	names := []string{"session_token_info", "rebind_session", "unknown-compatibility-probe"}
	for _, tool := range listed {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}
func releaseIsCompatibilityRefusal(result *mcp.CallToolResult, err error) bool {
	if err != nil || result == nil || !result.IsError {
		return false
	}
	raw, _ := json.Marshal(result.Content)
	return strings.Contains(string(raw), "client-incompatible:") && strings.Contains(string(raw), "required minimum 0.17.3")
}

func (f *releaseFixture) compatibilityRefused(version string, binary string) {
	t := f.t
	f.prepareClient()
	peer := releaseHTTPPeer(t, f.browser.origin, f.activeToken, version)
	defer peer.close()
	tools, err := peer.session.ListTools(t.Context(), nil)
	matrixRequire(t, err == nil && len(tools.Tools) > 0, "release-refused-list-remains")
	for _, name := range releaseRefusalInventory(tools.Tools) {
		result, e := peer.session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		matrixRequire(t, releaseIsCompatibilityRefusal(result, e), "release-below-minimum-refusal")
	}
	if binary != "" {
		c := matrixStart(t, binary, f.helper, f.browser.origin+"/mcp?project=test", f.activeToken, f.auth, f.home, f.cwd, f.sid)
		defer c.close()
		var status struct{ Registered bool }
		// An explicit old client must never obtain a registration on the candidate.
		result, e := c.session.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
		matrixRequire(t, releaseIsCompatibilityRefusal(result, e), "release-old-binary-compatibility-refused")
		matrixRequire(t, c.tool(t.Context(), "auth_status", map[string]any{}, &status) && !status.Registered, "release-old-binary-unregistered")
	}
}

func TestReleaseCandidateCompatibility(t *testing.T) {
	releaseGate(t)
	lock, manifest, candidate := releaseInputs(t, true)
	var build struct{ SHA256 map[string]string }
	raw, e := os.ReadFile(os.Getenv("ASB_RC_BUILD_EVIDENCE"))
	matrixRequire(t, e == nil && json.Unmarshal(raw, &build) == nil, "release-client-build-evidence")
	client := os.Getenv("ASB_RC_CANDIDATE_CLIENT")
	releaseVerifyBinary(t, client, build.SHA256["client"], "client", lock.Candidate)
	policy, ok := protocol.ServerPolicy(lock.Candidate)
	matrixRequire(t, ok, "release-candidate-policy")
	var full struct{ BelowMinimum string }
	raw, e = os.ReadFile(filepath.Join("..", "..", "..", "scripts", "release", "compat-assets.json"))
	matrixRequire(t, e == nil && json.Unmarshal(raw, &full) == nil, "release-below-minimum-lock")
	expected, passed := releaseCompatibilityCases(), []string{}
	for _, name := range expected {
		if !t.Run(name, func(t *testing.T) {
			server, clientBinary, serverVersion := candidate, client, lock.Candidate
			switch name {
			case "client/previous":
				clientBinary = releaseBinary(t, manifest, "client", lock.Previous)
			case "client/oldest":
				clientBinary = releaseBinary(t, manifest, "client", policy.MinClient)
			case "server/previous":
				server = releaseBinary(t, manifest, "server", lock.Previous)
				serverVersion = lock.Previous
			}
			if name == "client/below-minimum" || name == "client/placeholder" {
				f := releaseNew(t, serverVersion, server, clientBinary)
				f.compatibilitySetup()
				if name == "client/below-minimum" {
					f.compatibilityRefused(full.BelowMinimum, releaseBinary(t, manifest, "client", full.BelowMinimum))
				} else {
					f.compatibilityRefused("0.1.0", "")
				}
			} else {
				for _, mode := range []string{"manual", "auto"} {
					if !t.Run(mode, func(t *testing.T) {
						f := releaseNew(t, serverVersion, server, clientBinary)
						f.compatibilitySetup()
						f.compatibilityAllowed(lock.Candidate, mode)
					}) {
						return
					}
				}
			}
		}) {
			return
		}
		passed = append(passed, name)
	}
	releaseEvidence(t, "compatibility", expected, passed)
}

func TestReleaseCompatibilityCaseContract(t *testing.T) {
	want := []string{"client/below-minimum", "client/oldest", "client/placeholder", "client/previous", "server/previous"}
	got := releaseCompatibilityCases()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatal("release compatibility case contract changed")
	}
}

func TestReleaseRefusalInventoryRejectsHiddenBypass(t *testing.T) {
	names := releaseRefusalInventory([]*mcp.Tool{{Name: "list_sessions"}})
	good := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "client-incompatible: required minimum 0.17.3"}}}
	for _, hidden := range []string{"session_token_info", "rebind_session", "unknown-compatibility-probe"} {
		if !slices.Contains(names, hidden) {
			t.Fatal("hidden refusal probe missing")
		}
		rejected := false
		for _, name := range names {
			result := good
			if name == hidden {
				result = &mcp.CallToolResult{}
			}
			if !releaseIsCompatibilityRefusal(result, nil) {
				rejected = true
			}
		}
		if !rejected {
			t.Fatal("hidden compatibility bypass passed")
		}
	}
	if !releaseIsCompatibilityRefusal(good, nil) || releaseIsCompatibilityRefusal(good, errors.New("fixture")) || releaseIsCompatibilityRefusal(&mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "harness failure"}}}, nil) {
		t.Fatal("compatibility-specific refusal not enforced")
	}
}
