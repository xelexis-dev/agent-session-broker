package channel

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func (srv *ingressServer) discoverMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(callCtx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(callCtx, method, req)
		if method == "server/discover" && err == nil {
			srv.startPrune()
		}
		return result, err
	}
}

func (srv *ingressServer) degradationMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(callCtx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			if callReq, ok := req.(*mcp.CallToolRequest); ok && callReq.Params != nil {
				if thread, ok := callReq.Params.Meta["threadId"].(string); ok {
					srv.mu.Lock()
					if b := srv.bindings[thread]; b != nil {
						b.threadConfirmed = true
					}
					srv.mu.Unlock()
				}
			}

			if r, ok := req.(*mcp.CallToolRequest); ok && (!localReadTool(r.Params.Name) && r.Params.Name != "attach_session" && r.Params.Name != "report_bug") {
				id, err := srv.identify(callCtx, r)
				if err == nil {
					srv.mu.Lock()
					b := srv.bindings[id.SessionID]
					blocked := b != nil && b.receiveReason != ""
					srv.mu.Unlock()
					if blocked {
						return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: claudeInboxRecovery}}}, nil
					}
				}
			}
		}
		if method == "tools/call" {
			if request, ok := req.(*mcp.CallToolRequest); ok && request.Params.Name == "report_bug" {
				id, _ := srv.identify(callCtx, request)
				srv.mu.Lock()
				binding := srv.bindings[id.SessionID]
				target := srv.upstreamFor(binding)
				srv.mu.Unlock()
				callCtx = context.WithValue(callCtx, bugDiagnosticsKey{}, srv.opts.bugDiagnostics(callCtx, binding, target))
			}
		}
		var stopped *ingressBinding
		if r, ok := req.(*mcp.CallToolRequest); ok && method == "tools/call" && r.Params.Name == "stop_session" && srv.opts.enabled {
			var in struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(r.Params.Arguments, &in) == nil {
				id, identifyErr := srv.identify(callCtx, r)
				srv.mu.Lock()
				b := srv.bindings[id.SessionID]
				if identifyErr == nil && b != nil && b.started && b.durable != nil {
					name, reg := b.identity.Name, b.durable.registrationID
					if in.Name == name || in.Name == id.SessionID || in.Name == reg || in.Name == broker.SelectorName+name || in.Name == broker.SelectorSession+id.SessionID || in.Name == broker.SelectorRegistration+reg {
						stopped = b
					}
				}
				srv.mu.Unlock()
			}
		}
		result, err := next(callCtx, method, req)
		if out, ok := result.(*mcp.CallToolResult); stopped != nil && err == nil && ok && !out.IsError {
			// Join the old stream before a new attach can reuse its transport.
			srv.mu.Lock()
			// A concurrent handover retirement must not become eligible for revival.
			stopped.selfStopped = stopped.selfStopped || !stopped.retired
			stopped.retired, stopped.failed = true, true
			cancelReceiver, done := stopped.cancel, stopped.done
			srv.mu.Unlock()
			srv.opts.detachedObserved(stopped)
			if cancelReceiver != nil {
				cancelReceiver()
			}
			if done != nil {
				select {
				case <-done:
				case <-callCtx.Done():
					return nil, callCtx.Err()
				}
			}
		}
		if method == "tools/call" && srv.opts.claudeFollow != nil {
			if r, ok := req.(*mcp.CallToolRequest); ok && r.Params.Name == "get_session" {
				if out, ok := result.(*mcp.CallToolResult); ok {
					if summary := srv.opts.claudeFollow.summary(); summary != "" {
						out.Content = append(out.Content, &mcp.TextContent{Text: summary})
					}
				}
			}
		}
		return result, err
	}
}

func (srv *ingressServer) tokenModeMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(callCtx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {

			if r, ok := req.(*mcp.CallToolRequest); ok && r.Params.Name == "xbroker_docs" {
				return next(callCtx, method, req)
			}
			if toolReq, ok := req.(*mcp.CallToolRequest); ok && toolReq.Params.Name == "auth_status" && srv.opts.device == nil && (upstreamRejected(srv.upstream) || terminalProjectError(projectTerminalUpstream(srv.upstream))) {
				return next(callCtx, method, req)
			}
			if toolReq, ok := req.(*mcp.CallToolRequest); ok && toolReq.Params.Name == "auth_status" {
				if id, err := srv.identify(callCtx, toolReq); err == nil {
					srv.mu.Lock()
					b := srv.bindings[id.SessionID]
					waiting := b != nil && b.holderWaiting
					if b != nil && b.durable != nil && b.durable.session != nil {
						a := b.durable.session
						a.mu.Lock()
						waiting = waiting || a.holderWaiting
						a.mu.Unlock()
					}
					srv.mu.Unlock()
					if waiting {
						return next(callCtx, method, req)
					}
				}
			}

			if toolReq, ok := req.(*mcp.CallToolRequest); ok && upstreamCompatibility(srv.currentUpstream(callCtx, toolReq)).Compatible && toolReq.Params.Name != "attach_session" && toolReq.Params.Name != "activate_channel" {
				if err := srv.autoAttach(callCtx, toolReq); err != nil && !errors.Is(err, errAutomaticStopped) && !errors.Is(err, errAuthorizationPending) {
					var localLock *localClientLockError
					if errors.As(err, &localLock) {
						return srv.opts.localLockResult(srv.ctx, srv.currentUpstream(callCtx, toolReq), toolReq.Params.Name, localLock), nil
					}
					var localState *localStateError
					if errors.As(err, &localState) {
						return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: localState.Error()}}}, nil
					}
					if terminalProjectError(err) {
						return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
					}
					if isCompatibilityError(err) {
						if !localReadTool(toolReq.Params.Name) {
							return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
						}
					} else {
						return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: identityFailureGuidance(err)}}}, nil
					}
				}
			}
		}
		result, err := next(callCtx, method, req)
		if method == "server/discover" && err == nil {
			srv.automatic()
		}
		return result, err
	}
}

func (srv *ingressServer) ingressHook(hookCtx context.Context, req *mcp.CallToolRequest, in hookInput) (*mcp.CallToolResult, map[string]any, error) {
	srv.mu.Lock()
	b := srv.bindings[in.SessionID]
	started := b != nil && b.started && !b.failed
	denied := b != nil && receiveDenied(srv.upstreamFor(b))
	srv.mu.Unlock()
	if denied {
		return nil, nil, errReceiveUnavailable
	}
	if !started {
		return nil, map[string]any{}, nil
	}
	if b.router == nil {
		return nil, nil, errors.New("ingress hook requires its bound Codex session")
	}
	thread, err := callerThreadID(req)
	if err != nil || thread != in.SessionID {
		return nil, nil, errors.New("ingress hook caller does not match the bound Codex session")
	}
	srv.mu.Lock()
	if memo := hookObservation(hookCtx, req); memo != nil && memo.observed && !memo.current(srv.bindings[in.SessionID]) {
		srv.mu.Unlock()
		return nil, nil, errors.New("ingress hook binding changed; retry with the current session")
	}
	if srv.bindings[in.SessionID] != b || b.retired || (b.durable != nil && b.durable.handedOver) {
		srv.mu.Unlock()
		return nil, nil, errors.New("ingress hook binding changed; retry with the current session")
	}
	if in.Event == "Interrupt" {
		callbacks, err := b.router.interruptHandoff(hookCtx)
		srv.mu.Unlock()
		err = finishQueueHandoffs(callbacks, err)
		if err != nil {
			srv.mu.Lock()
			id, registrationID := b.identity, ""
			target := srv.upstreamFor(b)
			if b.durable != nil {
				registrationID = b.durable.registrationID
			}
			srv.mu.Unlock()
			reportIngressFailure(target, id, registrationID)
		}
		return nil, map[string]any{}, err
	}
	out, done, failed, err := b.router.hookHandoff(in.Event, in.TurnID)
	srv.mu.Unlock()
	if err == nil && done == nil {
		_ = b.router.steerHookDecision(hookCtx)
	}
	if err == nil && done != nil && srv.opts.hookWrites != nil {
		return srv.opts.hookWrites.result(done, failed), out, nil
	}
	if done != nil && srv.opts.hookWrites == nil {
		_ = done()
	}
	return nil, out, err
}

func (srv *ingressServer) requestLifetimeMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(callCtx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		requestCtx, cancelRequest := context.WithCancel(callCtx)
		stop := context.AfterFunc(srv.ctx, cancelRequest)
		defer stop()
		defer cancelRequest()
		if method == "tools/call" {
			if toolReq, ok := req.(*mcp.CallToolRequest); ok {
				if srv.opts.enabled && srv.opts.tokenMode && srv.opts.codexClaim != nil {
					requestCtx = withCodexYieldCall(requestCtx, toolReq)
					srv.attachMu.Lock()
					err := confirmCodexClaim(requestCtx, toolReq, srv.opts, &srv.mu, srv.bindings, srv.upstreamFor)
					srv.attachMu.Unlock()
					if err != nil {
						return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
					}
				}
				if _, hook := hookRequestInput(toolReq); hook {
					requestCtx = context.WithValue(requestCtx, hookIdentityKey{}, &hookIdentityObservation{request: toolReq})
				}
			}
		}
		return next(requestCtx, method, req)
	}
}
