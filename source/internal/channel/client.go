package channel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/claude"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/codex"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// registerFunc performs the upstream registration for an observed identity
// whose Name and ChannelID are already set. nil selects legacy attach_session.
type registerFunc func(context.Context, broker.AttachRequest) (broker.Session, error)

// internalUpstreamTools are used by this client itself and never forwarded to
// the model.
var internalUpstreamTools = map[string]bool{
	"session_token_info": true,
	"attach_session":     true, "report_ingress_failure": true,
	"register_session": true, "resume_session": true, "renew_lease": true, "probe_response": true,
}

// upstreamRouter picks the upstream for one forwarded call. A nil upstream
// with a message answers the model without reaching the broker, for example
// while the calling conversation still waits for approval.
type upstreamRouter func(context.Context, *mcp.CallToolRequest) (Upstream, string, error)

// addClientToolsWithPolicy exposes the broker tools to the model. With
// nameOptional (token-protected projects) attach_session accepts no name
// because the admission grant assigns the nickname; the returned name is the
// one this client receives on.
func addClientToolsWithPolicy(server *mcp.Server, upstream Upstream, upstreamTools []*mcp.Tool, identify func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error), register registerFunc, nameOptional bool, route upstreamRouter, attachMu *sync.Mutex, preflight func(context.Context, *mcp.CallToolRequest) (func(context.Context) error, func(), error), attachmentRoute upstreamRouter, attached ...func(broker.AttachRequest, broker.Session) error) func(context.Context, *mcp.CallToolRequest, string, registerFunc) (broker.Session, error) {
	addCompatibilityMiddleware(server, func(ctx context.Context, req *mcp.CallToolRequest) Upstream {
		if route != nil {
			target, _, err := route(ctx, req)
			if err == nil && target != nil {
				return target
			}
		}
		return upstream
	})
	hideListedTool(server, "wait_for_message", nil)
	for _, tool := range upstreamTools {
		if internalUpstreamTools[tool.Name] || tool.Name == "rename_session" || tool.Name == "xbroker_docs" {
			continue
		}
		name := tool.Name
		forwarded := *tool
		if name == "send_message" {
			localAttachmentSchema(&forwarded)
		}
		if name == "list_sessions" {
			forwarded.OutputSchema = nil
		}
		server.AddTool(&forwarded, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			target := upstream
			if route != nil {
				resolved, denied, err := route(ctx, req)
				if err != nil {
					return nil, err
				}
				if resolved == nil {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: denied}}}, nil
				}
				target = resolved
			}
			if name == "wait_for_message" && receiveDenied(target) {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: errReceiveUnavailable.Error()}}}, nil
			}
			arguments := req.Params.Arguments
			if name == "report_bug" {
				var err error
				arguments, err = bugReportArguments(ctx, req, target)
				if err != nil {
					return nil, err
				}
			}
			if _, present := attachmentArgument(&mcp.CallToolParams{Name: name, Arguments: arguments}); present && attachmentRoute != nil {
				resolved, denied, e := attachmentRoute(ctx, req)
				if e != nil {
					return nil, e
				}
				if resolved == nil {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: denied}}}, nil
				}
				target = resolved
			}
			result, err := callWithRecipientExpectation(ctx, target, &mcp.CallToolParams{Name: name, Arguments: arguments})
			if unsupported := unsupportedBugReportTool(name, result, err); unsupported != nil {
				return unsupported, nil
			}
			if name == "retire_session" && retirementUnsupported(target, result, err) {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "unsupported: this broker does not provide retire_session (requires server 0.17.3)"}}}, nil
			}
			if err == nil && insufficientScope(result) {
				return result, nil
			}
			if upstreamRejected(target) {
				return nil, errors.New(upstreamCredentialGuidance(target, route != nil))
			}
			if err != nil {
				return nil, err
			}
			// MCP structuredContent must be a JSON object. Preserve the existing
			// text JSON representation for legacy array results such as sessions.
			if result.StructuredContent != nil {
				data, err := json.Marshal(result.StructuredContent)
				if err != nil {
					return nil, err
				}
				if len(data) == 0 || data[0] != '{' {
					if len(result.Content) == 0 {
						result.Content = []mcp.Content{&mcp.TextContent{Text: string(data)}}
					}
					result.StructuredContent = nil
				}
			}
			return result, nil
		})
	}
	if attachMu == nil {
		attachMu = &sync.Mutex{}
	}
	attach := func(ctx context.Context, req *mcp.CallToolRequest, name string, override registerFunc) (broker.Session, error) {
		attachMu.Lock()
		defer attachMu.Unlock()
		if strings.TrimSpace(name) != name || (name == "" && !nameOptional) {
			return broker.Session{}, errors.New("a nonblank session name without surrounding whitespace is required")
		}
		var prepare func(context.Context) error
		if preflight != nil && override == nil {
			var release func()
			var err error
			prepare, release, err = preflight(ctx, req)
			if release != nil {
				defer release()
			}
			if err != nil {
				return broker.Session{}, err
			}
		}
		for attempt := 0; attempt < 2; attempt++ {
			if prepare != nil {
				if err := prepare(ctx); err != nil {
					return broker.Session{}, err
				}
			}
			if prepare != nil {
				if err := ctx.Err(); err != nil {
					return broker.Session{}, err
				}
			}
			identity, err := identify(ctx, req)
			if err != nil {
				return broker.Session{}, err
			}
			identity.Name = name
			var result broker.Session
			registration := register
			if override != nil {
				registration = override
			}
			if registration != nil {
				result, err = registration(ctx, identity)
				if err != nil {
					if prepare != nil && attempt == 0 && errors.Is(err, errExplicitBindingStale) {
						continue
					}
					return broker.Session{}, err
				}
			} else {
				request := identity
				request.Capabilities = []string{broker.MessageSentAtCapability}
				if err := call(ctx, upstream, "attach_session", request, &result); err != nil {
					return broker.Session{}, err
				}
			}
			for _, notify := range attached {
				if err := notify(identity, result); err != nil {
					return broker.Session{}, err
				}
			}
			return result, nil
		}
		return broker.Session{}, errExplicitBindingStale
	}
	const description = "Register this current session only when the user asks; registration is not automaticDeliveryReady. Identity is observed locally. Conflicting names are not stolen. Use the returned name; see xbroker_docs names."
	if nameOptional {
		type tokenInput struct {
			Takeover string `json:"takeover,omitempty" jsonschema:"Optional Claude conversation handover from this client's own saved registration name; only when the user asks to reconnect that conversation"`
			Name     string `json:"name,omitempty" jsonschema:"Optional; this token-protected project assigns the communication name from the admission token, and any requested name is ignored"`
		}
		mcp.AddTool(server, &mcp.Tool{Name: "attach_session", Description: "Register this session only when the user asks, not at startup or after tool errors. Registration is not automaticDeliveryReady. Identity is observed locally; token projects assign the name. Registered sessions resume automatically. See xbroker_docs names."}, func(ctx context.Context, req *mcp.CallToolRequest, in tokenInput) (*mcp.CallToolResult, broker.Session, error) {
			ctx = context.WithValue(context.WithValue(ctx, manualExplicitAttachKey{}, true), takeoverContextKey{}, in.Takeover)
			result, err := attach(ctx, req, in.Name, nil)
			return nil, result, err
		})
		return attach
	}
	type input struct {
		Name string `json:"name" jsonschema:"Communication name for this current agent session"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "attach_session", Description: description}, func(ctx context.Context, req *mcp.CallToolRequest, in input) (*mcp.CallToolResult, any, error) {
		result, err := attach(ctx, req, in.Name, nil)
		if err != nil {
			var rejection broker.AttachRejection
			if errors.As(err, &rejection) {
				safe := broker.SafeAttachRejection(rejection.Code)
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: safe.Error()}}}, safe, nil
			}
		}
		return nil, result, err
	})
	return attach
}

func RunClient(ctx context.Context, args []string, version ...string) error {
	return runClientTransport(ctx, &mcp.StdioTransport{}, args, clientParentSource{}, version...)
}
func runClientTransport(ctx context.Context, transport mcp.Transport, args []string, parent clientParentSource, version ...string) error {
	parent = parent.withDefaults()
	ctx = mcpconfig.WithVersion(ctx, mcpconfig.Version(version...))
	options, err := parseClientOptions(args)
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, toolProfileKey{}, options.toolProfile)
	var shared Upstream
	var tools []*mcp.Tool
	opts := durableOptions{codexSteerOff: options.codexSteerOff, tokenMode: true, project: options.project, scope: brokerScope(options.endpoint, options.project)}
	opts.receiverIdentity = clientstate.NewReceiverIdentity(opts.scope)
	if options.deviceMode {
		// A project token never reaches /mcp. Each conversation obtains its own
		// session credential and upstream; the local MCP server starts at once
		// with the generated tool definitions so the harness sees a full list.
		state, err := clientstate.Open(options.stateDir)
		if err != nil {
			return err
		}
		opts.enabled, opts.state = true, state
		opts.device = &deviceAuthOptions{
			nickname:   options.nickname,
			authorizer: newDeviceAuthClient(options.endpoint, options.token, nil),
			clock:      realPollClock(),
			newUpstream: func(credential string) Upstream {
				u := newBrokerUpstream(ctx, options.endpoint)
				u.setBearerToken(credential)
				return u
			},
		}
		shared, tools = unavailableUpstream{}, snapshotTools()
	} else {
		trackerStore, trackerErr := clientstate.Open(options.stateDir)
		if trackerErr != nil {
			return trackerErr
		}
		tracker, trackerErr := newProjectTracker(trackerStore, opts.scope)
		if trackerErr != nil {
			return trackerErr
		}
		upstream := newBrokerUpstream(ctx, options.endpoint)
		upstream.projectState = tracker
		defer upstream.Close()
		upstream.setBearerToken(options.token)
		bootstrapCtx, stopBootstrap := context.WithCancel(ctx)
		bootstrapDone := make(chan struct{})
		go func() {
			defer close(bootstrapDone)
			_, bootstrapErr := initialBrokerTools(bootstrapCtx, upstream, (durableOptions{}).withDefaults().backoff)
			upstream.recordProjectTerminal(bootstrapErr)
		}()
		defer func() { stopBootstrap(); <-bootstrapDone }()
		// The generated catalog is local. Network/authentication failures are
		// resolved by the ordinary background registration/tool paths, never
		// by blocking the harness's initialize/tools/list handshake.
		opts = opts.withDefaults()
		shared, tools = opts.wrapReceive(upstream, options.token), snapshotTools()
		state, err := clientstate.Open(options.stateDir)
		if err != nil {
			return err
		}
		opts.enabled, opts.state = true, state
	}

	ancestors, detected, pid, diagnostic := observeClientParent(parent)
	serveCtx := ctx
	if diagnostic == nil {
		serveCtx = context.WithValue(ctx, launchHintKey{}, launcherHint(runtime.GOOS, ancestors, pid))
	}
	if diagnostic != nil {
		opts.identityFailure = errUnsupportedHarness
		opts.parentDiagnostic = diagnostic
	}
	if detected == "codex" {
		opts.harness = "codex"
	}
	if detected == "kimi" {
		cwd, err := os.Getwd()
		if err != nil {
			return errors.New("Kimi working directory is unavailable")
		}
		k := newKimiClient(pid, cwd)
		opts.kimi, opts.harness = k, "kimi"
		stop := k.listen(ctx, opts.state.Dir())
		defer stop()
		capability := newDeviceAuthClient(options.endpoint, "", nil)
		k.checkServer = func(c context.Context) error {
			if capability.kimiSupported.Load() {
				return nil
			}
			if _, _, e := capability.discoverCapabilities(c); e != nil {
				return e
			}
			if !capability.kimiSupported.Load() {
				return errKimiServer
			}
			return nil
		}
		opts.observe = func(c context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
			return k.identify(c, nil)
		}
		opts.parentProcessKey = func(c context.Context, pid int) (string, error) {
			if e := c.Err(); e != nil {
				return "", e
			}
			return kimiProcessKey(pid)
		}
		return ServeDurableIngress(serveCtx, transport, shared, tools, k.identify, opts)
	}

	if diagnostic == nil {
		if detected == "claude" {
			opts.harness = "claude"
			opts.observe = func(c context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
				return parent.inspectClaude(c, pid)
			}
			opts.parentProcessKey = parent.processKey
			resolve := cachedClaudeParent(pid, parent.inspectClaude)
			opts.claudeInitial = resolve
			// This placeholder is never registered or used as proof. Identity
			// is resolved only after the local MCP server is serving.
			identity := broker.AttachRequest{Harness: "claude", PID: pid}
			if os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET") == "" {
				return ServeDurableChannel(serveCtx, transport, shared, tools, identity, opts)
			}
			opts.claudeInboxFactory = deferredClaudeInbox(ctx, resolve, opts.observe, opts.parentProcessKey)
			identify := claudeConversationIdentity(identity, &opts)
			return ServeDurableIngress(serveCtx, transport, shared, tools, identify, opts)
		}
	}
	if opts.enabled {
		opts.observe = observeBoundAgent
		identify := func(c context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
			if opts.identityFailure != nil {
				return broker.AttachRequest{}, opts.identityFailure
			}
			id, err := callerThreadID(req)
			if err != nil {
				return broker.AttachRequest{Harness: "codex", PID: pid}, err
			}
			return codex.NewAdapter().ObserveExternal(c, id, pid)
		}
		if opts.identityFailure != nil {
			opts.observe = func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) {
				return broker.AttachRequest{}, opts.identityFailure
			}
		}
		return ServeDurableIngress(serveCtx, transport, shared, tools, identify, opts)
	}
	// Both branches above enable durable mode; the non-durable path remains only as a test fixture.
	return ServeIngress(serveCtx, transport, shared, tools, parentAgentIdentity)
}

// observeClaude re-reads the interactive Claude parent for a probe answer.
func observeClaude(ctx context.Context, _ broker.AttachRequest) (broker.AttachRequest, error) {
	observed, err := parentIdentity(ctx)
	observed.Harness = "claude"
	return observed, err
}

// observeBoundAgent re-verifies the bound parent for a probe answer without
// consulting the model: Codex is checked as the bound thread under the bound
// PID (the thread hint may have come from request metadata at registration),
// Claude through its exact interactive/background parent roster.
func observeBoundAgent(ctx context.Context, bound broker.AttachRequest) (broker.AttachRequest, error) {
	if bound.Harness == "codex" {
		observed, err := codex.NewAdapter().ObserveExternal(ctx, bound.SessionID, bound.PID)
		if err != nil {
			return broker.AttachRequest{}, err
		}
		observed.Harness = "codex"
		return observed, nil
	}
	return observeClaude(ctx, bound)
}

// ServeDurableChannel is the durable client for a Claude session without the
// native messaging socket: broker messages arrive as official channel events
// after the model acknowledges the activation token, while registration,
// recovery and probes work exactly as in ServeDurableIngress.
func ServeDurableChannel(ctx context.Context, transport mcp.Transport, upstream Upstream, tools []*mcp.Tool, identity broker.AttachRequest, opts durableOptions) error {
	opts = opts.withDefaults()
	upstream = opts.wrapReceive(upstream, "")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wire := &capturedTransport{Transport: transport}
	token := rand.Text()
	initialized, ready := make(chan struct{}), make(chan struct{})
	opts.ready = ready
	var initializedOnce, readyOnce sync.Once
	var nameMu sync.Mutex
	boundName := ""
	previousOptions := opts.serverOptions
	opts.serverOptions = func(options *mcp.ServerOptions) {
		if previousOptions != nil {
			previousOptions(options)
		}
		options.Capabilities.Experimental = map[string]any{"claude/channel": map[string]any{}}
		options.Instructions = clientInstructions + " Acknowledge only the actual initial channel event token."
		previous := options.InitializedHandler
		options.InitializedHandler = func(callCtx context.Context, req *mcp.InitializedRequest) {
			if previous != nil {
				previous(callCtx, req)
			}
			initializedOnce.Do(func() { close(initialized) })
		}
	}
	opts.bound = func(session broker.Session) {
		nameMu.Lock()
		boundName = session.Name
		nameMu.Unlock()
	}
	// In project-token mode the reply tool must send through the
	// conversation's own credential-bearing upstream.
	replyTarget := upstream
	opts.connected = func(u Upstream) {
		nameMu.Lock()
		replyTarget = u
		nameMu.Unlock()
	}
	opts.configure = func(server *mcp.Server) {
		type activationInput struct {
			Token string `json:"token"`
		}
		type activationResult struct {
			Activated bool `json:"activated"`
		}
		mcp.AddTool(server, &mcp.Tool{Name: "activate_channel", Description: "Internal session channel activation: acknowledge only the actual event token. Never call manually or poll; activation does not register a session."}, func(_ context.Context, _ *mcp.CallToolRequest, in activationInput) (*mcp.CallToolResult, activationResult, error) {
			nameMu.Lock()
			target := replyTarget
			nameMu.Unlock()
			if receiveDenied(target) {
				return nil, activationResult{}, errReceiveUnavailable
			}
			if in.Token != token {
				return nil, activationResult{}, errors.New("activation token mismatch")
			}
			readyOnce.Do(func() { close(ready) })
			return nil, activationResult{Activated: true}, nil
		})
		type replyInput struct {
			ExpectRegistrationID *string `json:"expectRegistrationId,omitempty" jsonschema:"Expected peer registration ID; use incoming fromRegistrationId for replies"`
			To                   string  `json:"to"`
			Body                 string  `json:"body"`
		}
		mcp.AddTool(server, &mcp.Tool{Name: "reply", Description: "Reply with one message using expectRegistrationId from incoming fromRegistrationId; mismatch refuses. Reply once; no ACK loops. Never send secrets."}, func(toolCtx context.Context, req *mcp.CallToolRequest, in replyInput) (*mcp.CallToolResult, broker.SendResult, error) {
			nameMu.Lock()
			name, target := boundName, replyTarget
			nameMu.Unlock()
			if name == "" {
				return nil, broker.SendResult{}, errors.New("this session is not registered")
			}
			if _, err := broker.RecipientExpectation(req.Params.Arguments); err != nil {
				return nil, broker.SendResult{Code: broker.CodeInvalid}, nil
			}
			var result broker.SendResult
			err := call(toolCtx, target, "send_message", broker.Message{ExpectRegistrationID: in.ExpectRegistrationID, From: name, To: in.To, Body: in.Body}, &result)
			return nil, result, err
		})
	}
	factory := func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return func(sinkCtx context.Context, message broker.Message) error {
			// Never consume before the model acknowledged the channel; the
			// broker keeps the message queued meanwhile.
			select {
			case <-ready:
			case <-sinkCtx.Done():
				return sinkCtx.Err()
			}
			message = broker.LocalMessage(message)
			return wire.notify(sinkCtx, message.Body, messageMetadata(message))
		}, nil, nil
	}
	identify := claudeConversationIdentity(identity, &opts)
	server, shutdown := newIngressServerWithOptions(ctx, upstream, tools, identify, factory, opts)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(callCtx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(callCtx, method, req)
			if method == "server/discover" && err == nil {
				initializedOnce.Do(func() { close(initialized) })
			}
			return result, err
		}
	})
	defer shutdown()
	defer cancel()
	downstream, err := server.Connect(ctx, wire, nil)
	if err != nil {
		return err
	}
	defer downstream.Close()
	closed := make(chan error, 1)
	go func() { closed <- downstream.Wait() }()
	select {
	case <-initialized:
	case err := <-closed:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	activationTimer := time.NewTimer(500 * time.Millisecond)
	defer activationTimer.Stop()
	for {
		select {
		case <-ready:
			return waitClosed(ctx, closed)
		case err := <-closed:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-activationTimer.C:
			if err := wire.notify(ctx, "Activate automatic broker delivery by calling activate_channel with the activation token in this event. This is connection setup, not a user work request. If already acknowledged, ignore this duplicate.", map[string]string{"activation": token}); err != nil {
				return err
			}
			activationTimer.Reset(30 * time.Second)
		}
	}
}

func waitClosed(ctx context.Context, closed <-chan error) error {
	select {
	case err := <-closed:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// brokerScope names the broker endpoint and project without any query value,
// so registration state never keys on a token.
func brokerScope(endpoint, project string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return project
	}
	return strings.ToLower(u.Scheme+"://"+u.Host) + u.Path + "?project=" + project
}

func projectFromBrokerURL(endpoint string) (string, bool) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false
	}
	projects := u.Query()["project"]
	if len(projects) != 1 {
		return "", false
	}
	return projects[0], true
}

func validateBrokerURL(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Path != "/mcp" || u.User != nil || u.Fragment != "" {
		return errors.New("client requires a project-scoped HTTP or HTTPS broker URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	projects := query["project"]
	if err != nil || len(projects) != 1 || !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`).MatchString(projects[0]) {
		return errors.New("broker URL requires one valid project")
	}
	return nil
}

func nearestAgent(ancestors []int, classify func(int) (string, error)) (string, int, error) {
	for _, pid := range ancestors {
		harness, err := classify(pid)
		if err != nil {
			break
		}
		if harness != "" {
			return harness, pid, nil
		}
	}
	return "", 0, errUnsupportedHarness
}

var callerThreadUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func callerThreadID(req *mcp.CallToolRequest) (string, error) {
	var raw any = os.Getenv("CODEX_THREAD_ID")
	if req != nil && req.Params != nil {
		if value, present := req.Params.Meta["threadId"]; present {
			raw = value
		}
	}
	id, ok := raw.(string)
	if !ok || !callerThreadUUID.MatchString(id) {
		return "", errors.New("current Codex request has no valid thread identity")
	}
	return id, nil
}

func parentAgentIdentity(ctx context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ancestors, err := ancestorPIDs()
	if err != nil {
		return broker.AttachRequest{}, err
	}
	harness, pid, err := nearestAgent(ancestors, processHarness)
	if err != nil {
		return broker.AttachRequest{}, err
	}
	if harness == "codex" {
		id, err := callerThreadID(req)
		if err != nil {
			return broker.AttachRequest{Harness: "codex", PID: pid}, err
		}
		return codex.NewAdapter().ObserveExternal(ctx, id, pid)
	}
	return inspectClaudeParent(ctx, &claude.CLIExecutor{}, pid)
}
