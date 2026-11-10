package channel

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// The tracker belongs to one credential/endpoint upstream. Request order fences
// older replies even when the scopes make an A -> B -> A round trip.
type credentialScopeTracker struct {
	mu                                     sync.Mutex
	session                                *mcp.ClientSession
	incarnation, issued, accepted, version uint64
	source, generation                     string
	observation                            atomic.Pointer[credentialScopeSnapshot]
}
type credentialScopeSnapshot struct {
	version uint64
	scopes  []string
}
type credentialScopeStamp struct {
	incarnation, request uint64
	source, generation   string
}

func (t *credentialScopeTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.incarnation++
	t.session = nil
	t.version++
	if t.observation.Load() != nil {
		t.observation.Store(&credentialScopeSnapshot{version: t.version})
	}
}
func (t *credentialScopeTracker) initialized(s *mcp.ClientSession, source, generation string, result *mcp.InitializeResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.incarnation++
	t.session = s
	t.source = source
	t.generation = generation
	t.accepted = 0
	t.version++
	var scopes []string
	if source != "" && result != nil && result.Capabilities != nil {
		raw, _ := json.Marshal(result.Capabilities.Experimental[protocol.CredentialScopesCapability])
		var payload struct {
			Scopes json.RawMessage `json:"scopes"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			scopes = decodeCredentialScopes(payload.Scopes)
		}
	}
	t.observation.Store(&credentialScopeSnapshot{version: t.version, scopes: scopes})
}
func decodeCredentialScopes(raw json.RawMessage) []string {
	var scopes []string
	if json.Unmarshal(raw, &scopes) != nil || !access.ValidScopes(scopes) {
		return nil
	}
	return scopes
}
func (t *credentialScopeTracker) issue(s *mcp.ClientSession, identity ...string) credentialScopeStamp {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(identity) == 2 && s == t.session && identity[0] == t.source && identity[1] != t.generation {
		t.generation = identity[1]
		t.incarnation++
		t.version++
		t.observation.Store(&credentialScopeSnapshot{version: t.version})
	}
	t.issued++
	return credentialScopeStamp{incarnation: t.incarnation, request: t.issued, source: t.source, generation: t.generation}
}
func (t *credentialScopeTracker) currentLocked(s *mcp.ClientSession, stamp credentialScopeStamp) bool {
	return s == t.session && stamp.incarnation == t.incarnation && stamp.source == t.source && stamp.generation == t.generation && stamp.request >= t.accepted
}
func (t *credentialScopeTracker) current(s *mcp.ClientSession, stamp credentialScopeStamp) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.currentLocked(s, stamp)
}
func (t *credentialScopeTracker) result(s *mcp.ClientSession, stamp credentialScopeStamp, result *mcp.CallToolResult, source, generation string) {
	if result == nil || result.IsError || source == "" || source != stamp.source || generation != stamp.generation {
		return
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return
	}
	var payload struct {
		Code   string          `json:"code"`
		Scopes json.RawMessage `json:"credentialScopes"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Code != "" {
		return
	}
	absent := len(payload.Scopes) == 0
	scopes := decodeCredentialScopes(payload.Scopes)
	if scopes == nil && !absent {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.currentLocked(s, stamp) {
		return
	}
	t.accepted = stamp.request
	if absent {
		if previous := t.observation.Load(); previous == nil || previous.scopes == nil {
			return
		}
	}
	t.version++
	t.observation.Store(&credentialScopeSnapshot{version: t.version, scopes: scopes})
}
func (u *brokerUpstream) receiveScopeObservation() (uint64, []string) {
	v := u.credentialScopes.observation.Load()
	if v == nil {
		return 0, nil
	}
	return v.version, append([]string(nil), v.scopes...)
}

// Cache only the same accepted observation used by reception. Reads under the
// binding lock never acquire the authorization lock as a side effect.
func (o durableOptions) updateCredentialScopes(b *ingressBinding, u Upstream) {
	b.mu.Lock()
	var a *sessionAuth
	if b.durable != nil {
		a = b.durable.session
	}
	credential := b.upstreamCredential
	b.mu.Unlock()
	if a == nil || credential == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Credential == nil || a.state.Credential.AccessToken != credential {
		return
	}
	if scopes := observedCredentialScopes(u); scopes != nil {
		a.state.Credential.Scopes = append([]string(nil), scopes...)
	}
}
