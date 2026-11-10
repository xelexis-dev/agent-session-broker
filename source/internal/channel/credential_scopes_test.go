package channel

import (
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"strings"
	"testing"
)

func scopeInitialization(scopes any) *mcp.InitializeResult {
	return &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Experimental: map[string]any{"credential-scopes-v1": map[string]any{"scopes": scopes}}}}
}
func scopeSuccess(scopes any) *mcp.CallToolResult {
	return &mcp.CallToolResult{StructuredContent: map[string]any{"credentialScopes": scopes}}
}
func TestCredentialScopesValidateAuthenticateAndFenceObservations(t *testing.T) {
	allow := []string{access.MessageReceive, access.SessionRegister}
	deny := []string{access.SessionRegister}
	a, b := &mcp.ClientSession{}, &mcp.ClientSession{}
	var tracker credentialScopeTracker
	tracker.initialized(a, "credential-a", "generation-a", scopeInitialization(deny))
	initial := tracker.observation.Load()
	if len(initial.scopes) != 1 {
		t.Fatal("authenticated startup scope missing")
	}
	old := tracker.issue(a)
	newer := tracker.issue(a)
	tracker.result(a, newer, scopeSuccess(allow), "credential-a", "generation-a")
	latest := tracker.observation.Load()
	tracker.result(a, old, scopeSuccess(deny), "credential-a", "generation-a")
	if tracker.observation.Load() != latest || tracker.current(a, old) {
		t.Fatal("late response/denial replaced newer accepted scopes")
	}
	// A -> B -> A is ordered by the observation, never by scope-list equality.
	middle := tracker.issue(a)
	last := tracker.issue(a)
	tracker.result(a, last, scopeSuccess(deny), "credential-a", "generation-a")
	tracker.result(a, middle, scopeSuccess(allow), "credential-a", "generation-a")
	if access.HasScope(tracker.observation.Load().scopes, access.MessageReceive) {
		t.Fatal("A-B-A out-of-order reply restored B")
	}
	for _, tc := range []struct {
		name   string
		scopes any
	}{
		{"absent", nil}, {"empty", []string{}}, {"duplicate", []string{access.SessionRegister, access.SessionRegister}}, {"unknown", []string{"unknown:scope"}}, {"unordered", []string{access.SessionRegister, access.MessageReceive}}, {"wrong-type", "message:receive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := tracker.observation.Load()
			stamp := tracker.issue(a)
			tracker.result(a, stamp, scopeSuccess(tc.scopes), "credential-a", "generation-a")
			if tracker.observation.Load() != prior {
				t.Fatal("malformed scopes accepted")
			}
		})
	}
	for _, tc := range []struct {
		source, generation string
		result             *mcp.CallToolResult
	}{
		{"credential-b", "generation-a", scopeSuccess(allow)},
		{"credential-a", "generation-b", scopeSuccess(allow)},
		{"credential-a", "generation-a", &mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{"credentialScopes": allow}}},
		{"credential-a", "generation-a", &mcp.CallToolResult{StructuredContent: map[string]any{"code": "suspended", "credentialScopes": allow}}},
	} {
		prior := tracker.observation.Load()
		tracker.result(a, tracker.issue(a), tc.result, tc.source, tc.generation)
		if tracker.observation.Load() != prior {
			t.Fatal("unsuccessful or wrong credential/project generation observation accepted")
		}
	}
	stale := tracker.issue(a)
	tracker.initialized(b, "credential-a", "generation-a", scopeInitialization(deny))
	prior := tracker.observation.Load()
	tracker.result(a, stale, scopeSuccess(allow), "credential-a", "generation-a")
	if tracker.current(a, stale) || tracker.observation.Load() != prior {
		t.Fatal("old incarnation escaped fence")
	}
	tracker.initialized(b, "credential-a", "generation-a", &mcp.InitializeResult{})
	if tracker.observation.Load().scopes != nil {
		t.Fatal("downgrade guessed full access")
	}
	tracker.initialized(b, "", "generation-a", scopeInitialization(allow))
	if tracker.observation.Load().scopes != nil {
		t.Fatal("unauthenticated initialize taught scopes")
	}
	tracker.result(b, tracker.issue(b), scopeSuccess(allow), "", "generation-a")
	if tracker.observation.Load().scopes != nil {
		t.Fatal("unauthenticated result taught scopes")
	}
	tracker.reset()
	if tracker.current(b, tracker.issue(b)) {
		t.Fatal("credential reset retained old connection")
	}
}

func TestReceiveScopeEpochIgnoresLateDenialAndKeepsOldServerSticky(t *testing.T) {
	inner := &heartbeatScopeFixture{version: 1, scopes: []string{access.MessageReceive, access.SessionRegister}}
	opts := (durableOptions{}).withDefaults()
	u := opts.wrapReceive(inner, "credential").(*receiveUpstream)
	old := u.currentPermission()
	inner.mu.Lock()
	inner.version++
	inner.scopes = []string{access.SessionRegister}
	inner.mu.Unlock()
	denied := u.currentPermission()
	if denied == old || !receiveDenied(u) {
		t.Fatal("removal did not create forbidden epoch")
	}
	inner.mu.Lock()
	inner.version++
	inner.scopes = []string{access.MessageReceive, access.SessionRegister}
	inner.mu.Unlock()
	restored := u.currentPermission()
	old.decide(receiveForbidden)
	if restored == denied || receiveDenied(u) {
		t.Fatal("late denial poisoned restored epoch")
	}
	// A missing field/capability carries unknown, which cannot clear a real denial.
	restored.decide(receiveForbidden)
	inner.mu.Lock()
	inner.version++
	inner.scopes = nil
	inner.mu.Unlock()
	if !receiveDenied(u) {
		t.Fatal("downgrade cleared sticky denial")
	}
	// A replacement credential/connection owns a fresh epoch, not an old callback.
	replacement := opts.wrapReceive(inner, "credential").(*receiveUpstream)
	inner.mu.Lock()
	inner.version++
	inner.scopes = []string{access.MessageReceive, access.SessionRegister}
	inner.mu.Unlock()
	_ = u.currentPermission()
	if !receiveDenied(u) || receiveDenied(replacement) {
		t.Fatal("superseded wrapper updated current credential epoch")
	}
	other := opts.wrapReceive(&heartbeatScopeFixture{version: 1, scopes: []string{access.SessionRegister}}, "other-credential")
	if !receiveDenied(other) || receiveDenied(replacement) {
		t.Fatal("another token's observation leaked")
	}
}

func TestCredentialScopeParserRejectsMalformedJSON(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `[1]`, `["session:register","session:register"]`, `["` + strings.Repeat("x", 200) + `"]`} {
		if decodeCredentialScopes(json.RawMessage(raw)) != nil {
			t.Fatal("malformed list accepted")
		}
	}
}

func TestCredentialScopeUnknownResultAndGenerationUpgradeFence(t *testing.T) {
	var tracker credentialScopeTracker
	s := &mcp.ClientSession{}
	allow := []string{access.MessageReceive, access.SessionRegister}
	tracker.initialized(s, "credential", "old", scopeInitialization(allow))
	old := tracker.issue(s)
	latest := tracker.issue(s)
	tracker.result(s, latest, &mcp.CallToolResult{StructuredContent: map[string]any{}}, "credential", "old")
	if tracker.observation.Load().scopes != nil {
		t.Fatal("missing field retained stale positive scope observation")
	}
	tracker.result(s, old, scopeSuccess(allow), "credential", "old")
	if tracker.observation.Load().scopes != nil {
		t.Fatal("late reply revived a downgraded capability")
	}
	prior := tracker.issue(s)
	upgraded := tracker.issue(s, "credential", "new")
	if tracker.current(s, prior) {
		t.Fatal("authenticated generation transition retained earlier denial/observation")
	}
	tracker.result(s, upgraded, scopeSuccess(allow), "credential", "new")
	if !access.HasScope(tracker.observation.Load().scopes, access.MessageReceive) {
		t.Fatal("current generation could not publish its authenticated scope snapshot")
	}
	for i := 0; i < 50; i++ {
		stale := tracker.issue(s)
		current := tracker.issue(s)
		tracker.result(s, current, scopeSuccess([]string{access.SessionRegister}), "credential", "new")
		tracker.result(s, stale, scopeSuccess(allow), "credential", "new")
		if access.HasScope(tracker.observation.Load().scopes, access.MessageReceive) {
			t.Fatal("rapid old observations won")
		}
	}
}
