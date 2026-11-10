package channel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// brokerUpstream replaces failed HTTP MCP connections on subsequent calls.
// It never replays a call: losing a response does not prove a mutation failed.
type brokerUpstream struct {
	credentialScopes             credentialScopeTracker
	projectState                 *projectTracker
	projectSource, projectParent string
	expectedProjectGeneration    string
	projectFailure               atomic.Pointer[projectRefusalError]
	ctx                          context.Context
	serverVersion                string
	compatibility                compatibilityState
	cancel                       context.CancelFunc
	endpoint                     string
	header                       http.Header
	policy                       *httpPolicyGuard
	mu                           sync.Mutex
	current                      *mcp.ClientSession
	activitySession              *mcp.ClientSession
	activitySchema               bool
	runtimeInfoSchema            bool
	startupTransient             atomic.Bool
	suspended                    atomic.Bool
	rejected                     atomic.Bool
	// transport overrides http.DefaultTransport (tests with private TLS roots).
	transport http.RoundTripper
}

// verifyActivity is called after register/resume, never on the heartbeat's
// critical path. A replacement connection cannot inherit this schema gate.
func (u *brokerUpstream) verifyActivity(ctx context.Context) bool {
	s, err := u.session(ctx)
	if err != nil {
		return false
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var tools []*mcp.Tool
	cursor := ""
	for pages := 0; pages < 16; pages++ {
		listed, err := s.ListTools(check, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil || len(tools)+len(listed.Tools) > 256 {
			return false
		}
		tools = append(tools, listed.Tools...)
		cursor = listed.NextCursor
		if cursor == "" {
			allowed := activitySchemaAllowed(tools)
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.current != s {
				return false
			}
			u.activitySession, u.activitySchema = s, allowed
			u.runtimeInfoSchema = optionalRuntimeSchemaAllowed(tools, "runtimeInfo")
			return allowed
		}
	}
	return false
}
func (u *brokerUpstream) runtimeInfoAllowed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.current != nil && u.current == u.activitySession && u.runtimeInfoSchema
}
func (u *brokerUpstream) activityAllowed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.current != nil && u.current == u.activitySession && u.activitySchema
}

// credentialRejected reports that the broker answered 401 to this upstream's
// bearer: the session credential was revoked, detached or expired.
func (u *brokerUpstream) credentialRejected() bool { return u.rejected.Load() }

func newBrokerUpstream(ctx context.Context, endpoint string) *brokerUpstream {
	ctx, cancel := context.WithCancel(ctx)
	return &brokerUpstream{ctx: ctx, cancel: cancel, endpoint: endpoint, compatibility: compatibilityState{Compatible: true}}
}

// setBearerToken attaches the project admission token to every upstream HTTP
// request. The token never enters MCP arguments, results, logs or errors.
func (u *brokerUpstream) setBearerToken(token string) {
	u.credentialScopes.reset()
	u.mu.Lock()
	defer u.mu.Unlock()
	if token == "" {
		u.projectSource = ""
		u.header = nil
		u.policy = nil
		return
	}
	u.projectSource = credentialSource(token)
	u.header = http.Header{"Authorization": []string{"Bearer " + token}}
	if isProjectToken(token) || strings.HasPrefix(token, sessionTokenPrefix) {
		u.policy = newHTTPPolicyGuard(u.endpoint, &http.Client{Transport: u.transport})
	} else {
		u.policy = nil
	}
}

type headerTransport struct {
	projectState   *projectTracker
	projectSource  string
	projectFailure *atomic.Pointer[projectRefusalError]
	header         http.Header
	next           http.RoundTripper
	transient      *atomic.Bool
	suspended      *atomic.Bool
	rejected       *atomic.Bool
	policy         *httpPolicyGuard
}

func (t headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.projectState.terminal(t.projectSource); err != nil {
		return nil, err
	}
	if t.policy != nil && request.URL.Scheme == "http" {
		if err := t.policy.ensure(request.Context()); err != nil {
			var project *projectRefusalError
			if errors.As(err, &project) && t.projectFailure != nil {
				t.projectFailure.Store(project)
			}
			var transient *httpPolicyTransientError
			var retryable *directTransientError
			if t.transient != nil && (errors.As(err, &transient) || errors.As(err, &retryable) || (project != nil && !project.terminal)) {
				t.transient.Store(true)
			}
			return nil, err
		}
	}
	cloned := request.Clone(request.Context())
	for key, values := range t.header {
		cloned.Header[key] = append([]string(nil), values...)
	}
	if t.policy != nil && cloned.URL.Scheme == "http" {
		cloned.Header.Set("X-ASB-Client-Transport", "http")
	}
	if hint := t.projectState.hint(t.projectSource); hint != "" {
		cloned.Header.Set(protocol.ProjectGenerationHeader, hint)
	}
	response, err := t.next.RoundTrip(cloned)
	if err == nil && response.StatusCode >= 400 && response.StatusCode < 500 {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(body))
		if readErr == nil {
			if refusal := t.projectState.refusal(t.projectSource, response.StatusCode, body); refusal != nil {
				if t.transient != nil && !terminalProjectError(refusal) {
					t.transient.Store(true)
				}
				var p *projectRefusalError
				if errors.As(refusal, &p) && t.projectFailure != nil {
					t.projectFailure.Store(p)
				}
				return response, nil
			}
		}
		if projectTransientStatus(response.StatusCode) && t.projectState.hint(t.projectSource) != "" && t.transient != nil {
			t.transient.Store(true)
		}
	}
	// Startup may wait through a transport outage, but permanent HTTP, TLS
	// trust and policy errors still fail immediately without looping.
	if t.transient != nil {
		var network *net.OpError
		if (err != nil && (errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))) || (err == nil && response.StatusCode >= 500) {
			t.transient.Store(true)
		}
	}

	if err == nil && t.suspended != nil && response.StatusCode == http.StatusLocked {
		// Another concurrently completing MCP request must not erase a 423.
		t.suspended.Store(true)
	}
	if err == nil && t.rejected != nil && response.StatusCode == http.StatusUnauthorized {
		t.rejected.Store(true)
	}
	return response, err
}

func (u *brokerUpstream) session(ctx context.Context) (*mcp.ClientSession, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.projectState.terminal(u.projectSource); err != nil {
		return nil, err
	}
	if err := u.ctx.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if u.current != nil {
		return u.current, nil
	}
	// Only the handshake is bounded. Idle ingress waits retain their receiver
	// lifetime and are never periodically cancelled to implement polling.
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	stop := context.AfterFunc(u.ctx, cancel)
	defer stop()
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "agent-session-broker-client", Version: mcpconfig.ContextVersion(u.ctx)}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: u.endpoint}
	next := u.transport
	if next == nil {
		next = http.DefaultTransport
	}
	// Build the preflight client from the effective transport here. Callers may
	// install a private-root test or proxy transport before their first connect.
	policy := u.policy
	if policy != nil {
		policy = newHTTPPolicyGuard(u.endpoint, &http.Client{Transport: next})
		policy.projectState, policy.projectSource = u.projectState, u.projectSource
	}
	// headerTransport re-adds the bearer on every hop, so redirects are never
	// followed: the credential stays on the configured endpoint.
	transport.HTTPClient = &http.Client{Transport: headerTransport{projectState: u.projectState, projectSource: u.projectSource, projectFailure: &u.projectFailure, transient: &u.startupTransient, header: u.header, next: next, suspended: &u.suspended, rejected: &u.rejected, policy: policy}, CheckRedirect: noRedirect}
	sourceRevision, parentRevision := u.projectState.versions(u.projectSource, u.projectParent)
	s, err := c.Connect(connectCtx, transport, nil)
	if err != nil {
		if failure := u.projectFailure.Load(); failure != nil {
			return nil, failure
		}
		if terminal := u.projectState.terminal(u.projectSource); terminal != nil {
			return nil, terminal
		}
		return nil, errors.New("client could not connect to the broker")
	}
	u.projectFailure.Store(nil)
	u.projectState.initialized(u.projectSource, u.projectParent, s.InitializeResult(), sourceRevision, parentRevision)
	u.serverVersion = ""
	if initialized := s.InitializeResult(); initialized != nil && initialized.ServerInfo != nil {
		u.serverVersion = initialized.ServerInfo.Version
	}
	u.compatibility = negotiatedCompatibility(mcpconfig.ContextVersion(u.ctx), s.InitializeResult())
	scopeSource := ""
	if u.header.Get("Authorization") != "" {
		scopeSource = u.projectSource
	}
	scopeGeneration := u.projectState.generation(u.projectSource)
	scopeInitialization := s.InitializeResult()
	if u.expectedProjectGeneration != "" && scopeGeneration != "" && scopeGeneration != u.expectedProjectGeneration {
		scopeInitialization = nil
	}
	u.credentialScopes.initialized(s, scopeSource, scopeGeneration, scopeInitialization)
	u.current = s
	return s, nil
}

// initialBrokerTools lets a newly started local client wait for an
// administrator to lift suspension, or a temporarily unreachable server to
// return. Invalid credentials and permanent configuration errors are not retried.
func initialBrokerTools(ctx context.Context, u *brokerUpstream, backoff func(int) time.Duration) ([]*mcp.Tool, error) {
	for attempt := 0; ; attempt++ {
		u.suspended.Store(false)
		u.startupTransient.Store(false)
		initial, err := u.session(ctx)
		var tools []*mcp.Tool
		if err == nil {
			cursor := ""
			for {
				var listed *mcp.ListToolsResult
				listed, err = initial.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
				if err != nil {
					break
				}
				tools = append(tools, listed.Tools...)
				cursor = listed.NextCursor
				if cursor == "" {
					return tools, nil
				}
			}
		}
		if terminalProjectError(err) || u.rejected.Load() || (!u.suspended.Load() && !u.startupTransient.Load() && u.projectFailure.Load() == nil) {
			return nil, err
		}
		// A suspension may start between initialize and tools/list. Discard
		// that connection before retrying the handshake after the backoff.
		u.mu.Lock()
		old := u.current
		u.current = nil
		u.mu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
}

func (u *brokerUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	s, err := u.session(ctx)
	if err != nil {
		return nil, err
	}
	if u.expectedProjectGeneration != "" && u.projectState.generation(u.projectSource) != "" && u.projectState.generation(u.projectSource) != u.expectedProjectGeneration {
		return nil, u.projectState.refusal(u.projectSource, 410, []byte(`{"error":"project_deleted"}`))
	}
	if state := u.compatibilityInfo(); !state.Compatible && !localReadTool(p.Name) {
		return compatibilityResult(state), nil
	}
	// The connection can be replaced between sampling and sending. Strip the
	// optional input unless this exact session's schema was verified.
	if p.Name == "renew_lease" {
		u.mu.Lock()
		allowed := u.current == s && u.activitySession == s && u.activitySchema
		runtimeAllowed := u.current == s && u.activitySession == s && u.runtimeInfoSchema
		u.mu.Unlock()
		if !allowed || !runtimeAllowed {
			raw, err := json.Marshal(p.Arguments)
			if err != nil {
				return nil, err
			}
			var args map[string]json.RawMessage
			if err = json.Unmarshal(raw, &args); err != nil {
				return nil, err
			}
			if !allowed {
				delete(args, "activity")
			}
			if !runtimeAllowed {
				delete(args, "runtimeInfo")
			}
			copy := *p
			copy.Arguments = args
			p = &copy
		}
	}
	// Recheck ack support on the exact connection used by this call.
	if p.Name == "renew_lease" || p.Name == "wait_for_message" || p.Name == "register_session" || p.Name == "resume_session" {
		info := s.InitializeResult()
		if info == nil || info.Capabilities.Experimental[protocol.MessageAckCapability] != true {
			raw, err := json.Marshal(p.Arguments)
			if err != nil {
				return nil, err
			}
			var args map[string]json.RawMessage
			if json.Unmarshal(raw, &args) != nil {
				return nil, errors.New("invalid delivery request")
			}
			delete(args, "epoch")
			delete(args, "ackThrough")
			delete(args, "cursor")
			if p.Name == "wait_for_message" {
				delete(args, "registrationId")
			}
			if p.Name == "register_session" || p.Name == "resume_session" {
				var capabilities []string
				if raw, ok := args["capabilities"]; ok && json.Unmarshal(raw, &capabilities) == nil {
					kept := capabilities[:0]
					for _, capability := range capabilities {
						if capability != protocol.MessageAckCapability {
							kept = append(kept, capability)
						}
					}
					args["capabilities"], _ = json.Marshal(kept)
				}
			}
			copy := *p
			copy.Arguments = args
			p = &copy
		}
	}
	// Old servers validate additional input properties before decoding them.
	if p.Name == "attach_session" || p.Name == "register_session" || p.Name == "resume_session" {
		info := s.InitializeResult()
		supported := info != nil && info.Capabilities.Experimental[protocol.MessageSentAtCapability] == true
		if !supported {
			raw, err := json.Marshal(p.Arguments)
			if err != nil {
				return nil, err
			}
			var args map[string]json.RawMessage
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, err
			}
			delete(args, "capabilities")
			copy := *p
			copy.Arguments = args
			p = &copy
		}
	}
	if p.Name == "register_session" || p.Name == "resume_session" || p.Name == "rebind_session" {
		initialized := s.InitializeResult()
		if initialized == nil || initialized.Capabilities.Experimental[protocol.ReceiverHolderCapability] != true {
			raw, err := json.Marshal(p.Arguments)
			if err != nil {
				return nil, err
			}
			var args map[string]json.RawMessage
			if json.Unmarshal(raw, &args) != nil {
				return nil, errors.New("invalid holder request")
			}
			delete(args, "holder")
			delete(args, "holderProof")
			copy := *p
			copy.Arguments = args
			p = &copy
		}
	}
	// Recheck on the actual connection: reconnection may have negotiated an
	// older server since the local forwarder's capability check.
	if p.Name == "send_message" {
		raw, err := json.Marshal(p.Arguments)
		if err != nil {
			return recipientResult(protocol.CodeInvalid, ""), nil
		}
		expected, err := protocol.RecipientExpectation(raw)
		if err != nil {
			return recipientResult(protocol.CodeInvalid, ""), nil
		}
		info := s.InitializeResult()
		if p.Meta[protocol.AttachmentMetaKey] != nil && (info == nil || info.Capabilities.Experimental[protocol.AttachmentCapability] != true) {
			return recipientResult(protocol.CodeUnsupported, ""), nil
		}
		if expected != nil && (info == nil || info.Capabilities.Experimental[protocol.RecipientExpectationCapability] != true) {
			return recipientResult(protocol.CodeUnsupported, "expectRegistrationId is unsupported by this broker; no message was sent"), nil
		}
	}
	sourceRevision, parentRevision := u.projectState.versions(u.projectSource, u.projectParent)
	u.mu.Lock()
	scopeStamp := u.credentialScopes.issue(s, u.projectSource, u.projectState.generation(u.projectSource))
	u.mu.Unlock()
	result, err := s.CallTool(ctx, p)
	if err != nil && ctx.Err() == nil {
		if failure := u.projectFailure.Load(); failure != nil {
			err = failure
		}
	}
	if err == nil && p.Name == "wait_for_message" && insufficientScope(result) {
		u.mu.Lock()
		current := u.current == s && u.projectSource == scopeStamp.source && u.projectState.generation(u.projectSource) == scopeStamp.generation && u.projectState.terminal(u.projectSource) == nil && u.credentialScopes.current(s, scopeStamp)
		u.mu.Unlock()
		if !current {
			return nil, errStaleScopeObservation
		}
	}
	if err == nil && (p.Name == "register_session" || p.Name == "resume_session" || p.Name == "renew_lease") {
		u.mu.Lock()
		if u.current == s && u.projectState.terminal(u.projectSource) == nil {
			u.credentialScopes.result(s, scopeStamp, result, u.projectSource, u.projectState.generation(u.projectSource))
		}
		u.mu.Unlock()
	}
	if err == nil {
		if p.Name == "session_token_info" && result != nil && !result.IsError {
			raw, _ := json.Marshal(result.StructuredContent)
			var info access.TokenInfo
			if json.Unmarshal(raw, &info) == nil {
				u.projectState.tokenInfo(u.projectSource, u.projectParent, info, sourceRevision, parentRevision)
			}
		}
		u.mu.Lock()
		if u.current == s {
			if state, blocked := gateCompatibility(result, mcpconfig.ContextVersion(u.ctx), u.serverVersion, u.compatibility); blocked {
				u.compatibility = state
				result = compatibilityResult(state)
			}
		}
		u.mu.Unlock()
	}

	var rpcErr *jsonrpc.Error
	if err != nil && ctx.Err() == nil && !errors.As(err, &rpcErr) {
		// Application rejections do not poison the connection. Concurrent
		// errors from an old connection must never close its replacement.
		u.mu.Lock()
		if u.current == s {
			u.current = nil
		}
		u.mu.Unlock()
		_ = s.Close()
	}
	return result, err
}

func (u *brokerUpstream) Close() error {
	u.cancel()
	u.mu.Lock()
	s := u.current
	u.current = nil
	u.mu.Unlock()
	if s != nil {
		return s.Close()
	}
	return nil
}

// Used only for process-local permission memory; never exposed or persisted.
func (u *brokerUpstream) receiveCredentialKey() [32]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.header) == 0 {
		return [32]byte{}
	}
	// setBearerToken constructs this header from the credential without extra data.
	return sha256.Sum256([]byte(strings.TrimPrefix(u.header.Get("Authorization"), "Bearer ")))
}

// Suspension is latched by HTTP 423 until a definitive non-suspended resume
// response. Ordinary concurrent successes cannot clear this observation.
func (u *brokerUpstream) registrationSuspended() bool  { return u.suspended.Load() }
func (u *brokerUpstream) clearRegistrationSuspension() { u.suspended.Store(false) }
func upstreamSuspended(u Upstream) bool {
	v, ok := u.(interface{ registrationSuspended() bool })
	return ok && v.registrationSuspended()
}
func clearUpstreamSuspension(u Upstream) {
	if v, ok := u.(interface{ clearRegistrationSuspension() }); ok {
		v.clearRegistrationSuspension()
	}
}

func (u *brokerUpstream) verifyMessageAck(ctx context.Context) bool {
	s, err := u.session(ctx)
	if err != nil {
		return false
	}
	info := s.InitializeResult()
	return info != nil && info.Capabilities.Experimental[protocol.MessageAckCapability] == true
}
