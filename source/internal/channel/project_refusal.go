package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type projectRefusalError struct {
	code     string
	terminal bool
}

func (e *projectRefusalError) Error() string {
	if e.terminal {
		return e.code + ": The project was deleted or replaced. Check the broker URL, project and token with an administrator, then explicitly call attach_session. The saved registration has been preserved; no new registration was created."
	}
	return e.code + ": The project is temporarily unavailable; the client will retry."
}
func (e *projectRefusalError) Unwrap() error {
	if !e.terminal {
		return &directTransientError{retryAfter: 30 * time.Second}
	}
	return nil
}
func terminalProjectError(err error) bool {
	var p *projectRefusalError
	return errors.As(err, &p) && p.terminal
}
func credentialSource(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type projectTracker struct {
	mu         sync.Mutex
	stateError error
	revisions  map[string]uint64
	store      *clientstate.Store
	scope      string
	entries    map[string]clientstate.ProjectKnowledge
}

func newProjectTracker(store *clientstate.Store, scope string) (*projectTracker, error) {
	entries := map[string]clientstate.ProjectKnowledge{}
	if store != nil {
		var err error
		entries, err = store.LoadProjectKnowledge(scope)
		if err != nil {
			return &projectTracker{revisions: map[string]uint64{}, store: store, scope: scope, entries: map[string]clientstate.ProjectKnowledge{}, stateError: err}, err
		}
	}
	return &projectTracker{revisions: map[string]uint64{}, store: store, scope: scope, entries: entries}, nil
}
func (p *projectTracker) saveLocked() {
	if p.store != nil {
		if err := p.store.SaveProjectKnowledge(p.scope, p.entries); err != nil {
			p.stateError = errors.New("project knowledge could not be saved")
		}
	}
}
func (p *projectTracker) generation(source string) string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[source].GenerationID
}
func (p *projectTracker) hint(source string) string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.entries[source]
	if v.Supported {
		return v.GenerationID
	}
	return ""
}
func (p *projectTracker) versions(source, parent string) (uint64, uint64) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.revisions[source], p.revisions[parent]
}
func (p *projectTracker) learn(source, parent, generation string, supported bool, expected ...uint64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(expected) == 2 && (p.revisions[source] != expected[0] || p.revisions[parent] != expected[1]) {
		return
	}
	v := p.entries[source]
	if v.Terminal != "" {
		return
	}
	v.Supported = false
	if supported && len(generation) == 26 {
		v.GenerationID = generation
		v.Supported = true
	}
	p.entries[source] = v
	if parent != "" {
		parentValue := v
		if prior := p.entries[parent]; prior.Terminal != "" {
			parentValue.Terminal = prior.Terminal
		}
		p.entries[parent] = parentValue
	}
	p.saveLocked()
}
func (p *projectTracker) reset(source string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Keep prior-generation evidence, but never send it with an explicit recovery.
	p.revisions[source]++
	v := p.entries[source]
	v.Supported = false
	v.Terminal = ""
	p.entries[source] = v
	p.saveLocked()
}
func (p *projectTracker) terminal(source string) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stateError != nil {
		return p.stateError
	}
	v := p.entries[source]
	if v.Terminal != "" {
		return &projectRefusalError{v.Terminal, true}
	}
	return nil
}
func (p *projectTracker) refusal(source string, status int, body []byte) error {
	if p == nil {
		return nil
	}
	var payload protocol.ProjectRefusal
	if json.Unmarshal(body, &payload) != nil || protocol.ProjectRefusalStatus(payload.ErrorCode) != status || status == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.entries[source]
	// A refusal can never teach feature support or a generation.
	if !v.Supported {
		return &directTransientError{retryAfter: 30 * time.Second}
	}
	terminal := payload.ErrorCode == "project_deleted" || (payload.ErrorCode == "project_not_found" && v.GenerationID != "")
	if terminal {
		for key, known := range p.entries {
			if known.Supported && known.GenerationID == v.GenerationID {
				known.Terminal = payload.ErrorCode
				p.entries[key] = known
			}
		}
		p.saveLocked()
	}
	return &projectRefusalError{payload.ErrorCode, terminal}
}
func (p *projectTracker) initialized(source, parent string, result *mcp.InitializeResult, expected ...uint64) {
	if p == nil {
		return
	}
	generation := ""
	supported := false
	if result != nil && result.Capabilities != nil {
		raw, _ := json.Marshal(result.Capabilities.Experimental[protocol.ProjectRefusalCapability])
		var data struct {
			Generation string `json:"projectGenerationId"`
		}
		if json.Unmarshal(raw, &data) == nil && data.Generation != "" {
			generation = data.Generation
			supported = true
		}
	}
	p.learn(source, parent, generation, supported, expected...)
}
func (p *projectTracker) tokenInfo(source, parent string, info access.TokenInfo, expected ...uint64) {
	p.learn(source, parent, info.ProjectGenerationID, info.ProjectRefusals, expected...)
}
func projectTransientStatus(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone || status == http.StatusForbidden
}

func (o durableOptions) deviceAuthorizerClient() (*deviceAuthClient, bool) {
	if o.device == nil {
		return nil, false
	}
	a, ok := o.device.authorizer.(*deviceAuthClient)
	return a, ok
}
func (o durableOptions) projectTerminal(b *ingressBinding) error {
	auth, ok := o.deviceAuthorizerClient()
	if !ok {
		return nil
	}
	if b != nil && b.durable != nil && b.durable.session != nil {
		a := b.durable.session
		a.mu.Lock()
		credential := a.state.Credential
		a.mu.Unlock()
		if credential != nil {
			if err := auth.projectState.terminal(credentialSource(credential.AccessToken)); err != nil {
				return err
			}
		}
	}
	return auth.projectState.terminal(auth.projectSource)
}

// A changed admission source cannot inherit a hint or stop bound to its predecessor.
func (o durableOptions) resetSupersededProjectSource(a *sessionAuth) {
	auth, ok := o.deviceAuthorizerClient()
	if !ok {
		return
	}
	current := o.admissionFingerprint()
	if current == "" || a.state.AdmissionFingerprint == current || a.state.Credential == nil {
		return
	}
	previous := credentialSource(a.state.Credential.AccessToken)
	stopped := terminalProjectError(auth.projectState.terminal(previous))
	auth.projectState.reset(previous)
	if stopped {
		a.state.Credential = nil
		a.state.AutoRecovery = nil
		if a.state.Device == nil {
			a.state.Device = &clientstate.DeviceAuthorization{}
		}
		a.state.Device.Status = clientstate.DeviceRejected
	}
}

func projectUpstream(u Upstream) *brokerUpstream {
	if wrapped, ok := u.(*receiveUpstream); ok {
		return projectUpstream(wrapped.Upstream)
	}
	actual, _ := u.(*brokerUpstream)
	return actual
}
func rememberProjectRefusal(b *ingressBinding, u Upstream) {
	actual := projectUpstream(u)
	if actual == nil || actual.projectState == nil {
		return
	}
	b.mu.Lock()
	id := b.durable.registrationID
	b.mu.Unlock()
	if id == "" {
		return
	}
	p := actual.projectState
	p.mu.Lock()
	defer p.mu.Unlock()
	generation := actual.expectedProjectGeneration
	if generation == "" {
		generation = p.entries[actual.projectSource].GenerationID
	}
	p.entries["registration:"+id] = clientstate.ProjectKnowledge{GenerationID: generation, Terminal: "project_deleted"}
	p.saveLocked()
}
func resetProjectUpstream(u Upstream) {
	if actual := projectUpstream(u); actual != nil {
		actual.projectState.reset(actual.projectSource)
		actual.projectFailure.Store(nil)
	}
}
func projectRegistrationStopped(u Upstream, id string) bool {
	actual := projectUpstream(u)
	if actual == nil || actual.projectState == nil || id == "" {
		return false
	}
	p := actual.projectState
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries["registration:"+id].Terminal != ""
}

// Explicit recovery of a deleted project's saved proof must never repair by registering anew.
func registerProjectSafe(ctx context.Context, u Upstream, b *ingressBinding, o durableOptions, id protocol.AttachRequest) (protocol.Session, error) {
	b.mu.Lock()
	saved := b.durable.registrationID
	b.mu.Unlock()
	if !projectRegistrationStopped(u, saved) {
		return registerDurable(ctx, u, b, o, id)
	}
	if result, ok := resumeDurable(ctx, u, b, o, id.ChannelID); ok {
		actual := projectUpstream(u)
		p := actual.projectState
		p.mu.Lock()
		p.entries["registration:"+saved] = clientstate.ProjectKnowledge{}
		p.saveLocked()
		p.mu.Unlock()
		return result, nil
	}
	if ctx.Err() != nil {
		return protocol.Session{}, ctx.Err()
	}
	return protocol.Session{}, &projectRefusalError{code: "project_deleted", terminal: true}
}

// A newly issued child inherits only evidence authenticated for its own admission source.
func (p *projectTracker) inherit(source, parent string) {
	if p == nil || parent == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, known := p.entries[source]; known {
		return
	}
	if evidence := p.entries[parent]; evidence.Supported {
		p.entries[source] = evidence
		p.saveLocked()
	}
}

// Bootstrap errors stay local: discovery must not depend on broker availability.
func (u *brokerUpstream) recordProjectTerminal(err error) {
	var terminal *projectRefusalError
	if errors.As(err, &terminal) && terminal.terminal {
		u.projectFailure.Store(terminal)
	}
}
func projectTerminalUpstream(upstream Upstream) error {
	if actual := projectUpstream(upstream); actual != nil {
		if err := actual.projectState.terminal(actual.projectSource); err != nil {
			return err
		}
		if failure := actual.projectFailure.Load(); failure != nil && failure.terminal {
			return failure
		}
	}
	return nil
}
