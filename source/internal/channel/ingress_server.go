package channel

import (
	"context"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"sync"
	"time"
)

// ingressServer is the state shared by the local MCP server's handlers.
// newIngressServerWithOptions owns construction and the registration order;
// the methods were its closures and keep their bodies unchanged.
type ingressServer struct {
	ctx                 context.Context
	upstream            Upstream
	tools               []*mcp.Tool
	identify            func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)
	factory             ingressSinkFactory
	opts                durableOptions
	options             *mcp.ServerOptions
	mu                  sync.Mutex
	bindings            map[string]*ingressBinding
	discoverIdentity    func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)
	workers             sync.WaitGroup
	stopping            bool
	shutdownOnce        sync.Once
	pruneOnce           sync.Once
	previousInitialized func(context.Context, *mcp.InitializedRequest)
	server              *mcp.Server
	attachMu            sync.Mutex
	register            registerFunc
	route               upstreamRouter
	attach              func(context.Context, *mcp.CallToolRequest, string, registerFunc) (broker.Session, error)
	autoRegister        func(callCtx context.Context, identity broker.AttachRequest) (broker.Session, error)
	automaticOnce       sync.Once
}

// newIngressServerWithOptions returns the local MCP server and a shutdown
// function that waits for every receiver goroutine to finish and then releases
// the registration locks. It runs automatically once ctx ends and is safe to
// call again to join the shutdown.
func newIngressServerWithOptions(ctx context.Context, upstream Upstream, tools []*mcp.Tool, identify func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error), factory ingressSinkFactory, opts durableOptions) (*mcp.Server, func()) {
	srv := &ingressServer{ctx: ctx, upstream: upstream, tools: tools, identify: identify, factory: factory, opts: opts}
	srv.opts = srv.opts.withDefaults()
	if srv.opts.enabled && srv.opts.state != nil && srv.opts.harness == "codex" {
		srv.opts.codexClaim = &codexResumeClaim{}
		original := srv.identify
		srv.identify = func(c context.Context, r *mcp.CallToolRequest) (broker.AttachRequest, error) {
			return srv.opts.codexClaim.identify(c, r, srv.opts, original)
		}
	}
	// One scope tracker is shared by admission and its credential upstreams.
	if srv.opts.device != nil {
		if authorizer, ok := srv.opts.device.authorizer.(*deviceAuthClient); ok {
			if authorizer.projectState == nil {
				authorizer.projectState, _ = newProjectTracker(srv.opts.state, srv.opts.scope)
			}
			original := srv.opts.device.newUpstream
			srv.opts.device.newUpstream = func(credential string) Upstream {
				up := original(credential)
				if brokerUp, ok := up.(*brokerUpstream); ok {
					brokerUp.projectState = authorizer.projectState
				}
				return up
			}
			if authorizer.policy != nil {
				authorizer.policy.projectState = authorizer.projectState
				authorizer.policy.projectSource = authorizer.projectSource
			}
		}
	}

	if srv.opts.enabled {
		srv.upstream = srv.opts.wrapReceive(srv.upstream, "")
	}
	if srv.opts.enabled {
		srv.opts.activityPool = newActivityWorkers(srv.ctx)
	}
	srv.options = mcpconfig.ServerOptions()
	srv.options.Instructions = clientInstructions
	if srv.opts.serverOptions != nil {
		srv.opts.serverOptions(srv.options)
	}
	if srv.opts.kimi != nil {
		srv.options.Instructions += " Kimi delivery is turn-boundary: " + kimiDeliveryGuidance + ". Configure kimi-hook; do not poll or attempt idle wake."
	}
	srv.options.Instructions += versionInstructions + docsInstructions
	srv.bindings = map[string]*ingressBinding{}
	srv.discoverIdentity = srv.identify
	srv.identify = func(callCtx context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
		if srv.opts.identityFailure != nil {
			return broker.AttachRequest{}, srv.opts.identityFailure
		}
		return observeHookIdentity(callCtx, req, &srv.mu, srv.bindings, srv.discoverIdentity)
	}
	go srv.shutdown()
	srv.previousInitialized = srv.options.InitializedHandler
	srv.options.InitializedHandler = func(callCtx context.Context, req *mcp.InitializedRequest) {
		if srv.previousInitialized != nil {
			srv.previousInitialized(callCtx, req)
		}
		srv.startPrune()
	}
	if srv.opts.enabled && srv.opts.tokenMode && srv.opts.identityFailure == nil {
		previous := srv.options.InitializedHandler
		srv.options.InitializedHandler = func(callCtx context.Context, req *mcp.InitializedRequest) {
			if previous != nil {
				previous(callCtx, req)
			}
			srv.automatic()
		}
	}
	srv.server = mcp.NewServer(&mcp.Implementation{Name: "agent-session-broker", Version: mcpconfig.ContextVersion(srv.ctx)}, srv.options)
	addDocsTool(srv.server, mcpconfig.ContextVersion(srv.ctx))
	// Modern MCP uses server/discover instead of initialize/initialized.
	// Both handshakes schedule the same once-only background task.
	srv.server.AddReceivingMiddleware(srv.discoverMiddleware)

	if srv.opts.enabled {
		srv.register = srv.registerDurable
	}
	srv.register = srv.finishRegistration(srv.register)
	if srv.register != nil {
		explicitRegister := srv.register
		srv.register = func(callCtx context.Context, id broker.AttachRequest) (broker.Session, error) {
			result, err := explicitRegister(callCtx, id)
			if err == nil {
				srv.mu.Lock()
				b := srv.bindings[id.SessionID]
				stale := b == nil || b.failed || b.identity.ChannelID != id.ChannelID
				srv.mu.Unlock()
				if stale {
					err = errExplicitBindingStale
				}
			}
			if err != nil && !errors.Is(err, errAuthorizationPending) {
				srv.mu.Lock()
				b := srv.bindings[id.SessionID]
				srv.mu.Unlock()
				if b != nil && srv.opts.initialRegistrationPending(b) {
					if isCompatibilityError(err) || errors.Is(err, errAutomaticStopped) || errors.Is(err, errInsufficientScope) {
						srv.opts.stopInitialBinding(b, id)
					} else {
						srv.mu.Lock()
						registerPending := b.durable.pendingRequest != ""
						claude := b.identity.Harness == "claude"
						srv.mu.Unlock()
						pendingApproval := false
						if a := b.durable.session; a != nil {
							a.mu.Lock()
							pendingApproval = a.status == authPending
							a.mu.Unlock()
						}
						if registerPending || (claude && pendingApproval) {
							srv.continueInitial(b)
						}
					}
				}
			}
			return result, err
		}
	}
	// Degradation never changes credential scopes or lies about readiness.
	// Keep discovery/diagnostics available, but reject competing send/receive.
	srv.server.AddReceivingMiddleware(srv.degradationMiddleware)
	if srv.opts.device != nil {
		// Forwarded tools run on the calling conversation's own upstream; before
		// approval they answer with the pending code instead of reaching the broker.
		srv.route = srv.routeDeviceCall
	}
	srv.attach = addClientToolsWithPolicy(srv.server, srv.upstream, srv.tools, srv.prepare, srv.register, srv.opts.enabled && srv.opts.tokenMode, srv.route, &srv.attachMu, srv.prepareExplicitAttach, srv.routeForwardedCall, srv.attached)
	if srv.opts.enabled && hasTool(srv.tools, "rename_session") {
		type renameInput struct {
			Name string `json:"name" jsonschema:"New nickname for THIS connected session; duplicates are rejected"`
		}
		mcp.AddTool(srv.server, &mcp.Tool{Name: "rename_session", Description: "Rename this session; duplicate names are refused. Registration and queued messages remain. Use the returned name. See xbroker_docs names."}, func(callCtx context.Context, req *mcp.CallToolRequest, in renameInput) (*mcp.CallToolResult, broker.Session, error) {
			identity, err := srv.identify(callCtx, req)
			if err != nil {
				return nil, broker.Session{}, err
			}
			srv.mu.Lock()
			b := srv.bindings[identity.SessionID]
			if srv.stopping || b == nil || !b.started || b.failed || b.durable == nil || b.identity.PID != identity.PID || b.identity.Harness != identity.Harness || !sameCWD(b.identity.CWD, identity.CWD) {
				srv.mu.Unlock()
				return nil, broker.Session{}, errors.New("rename requires this conversation's active registration")
			}
			srv.workers.Add(1)
			srv.mu.Unlock()
			defer srv.workers.Done()
			callCtx, cancel := context.WithCancel(callCtx)
			stop := context.AfterFunc(srv.ctx, cancel)
			defer stop()
			defer cancel()
			b.registerMu.Lock()
			defer b.registerMu.Unlock()
			srv.mu.Lock()
			if srv.bindings[identity.SessionID] != b || !b.started || b.failed {
				srv.mu.Unlock()
				return nil, broker.Session{}, errors.New("registration changed while preparing nickname change; retry after reconnecting")
			}
			request := broker.RenameRequest{Name: in.Name, RegistrationID: b.durable.registrationID, ChannelID: b.identity.ChannelID}
			target := srv.upstreamFor(b)
			srv.mu.Unlock()
			var result broker.Session
			if err := call(callCtx, target, "rename_session", request, &result); err != nil {
				return nil, broker.Session{}, err
			}
			srv.mu.Lock()
			b.used = true
			b.identity.Name = result.Name
			b.durable.name, b.durable.revision = result.Name, result.Revision
			saved, bound := *b.durable, b.identity
			srv.mu.Unlock()
			if srv.opts.bound != nil {
				srv.opts.bound(result)
			}
			if err := srv.opts.save(bound, &saved); err != nil {
				return nil, broker.Session{}, errors.New("broker renamed the session but saving local state failed; retry the same nickname")
			}
			return nil, result, nil
		})
	}
	if srv.opts.device != nil {
		mcp.AddTool(srv.server, &mcp.Tool{Name: "auth_status", Description: "Inspect this session authorization, versions, liveness, scopes and recovery guidance. Follow explicit authorization steps when needed; see xbroker_docs errors/upgrade."}, srv.deviceAuthStatus)
	}
	if srv.opts.device == nil && srv.opts.tokenMode {
		mcp.AddTool(srv.server, &mcp.Tool{Name: "auth_status", Description: "Inspect this session authorization, versions, liveness, scopes and recovery guidance. Follow explicit authorization steps when needed; see xbroker_docs errors/upgrade."}, srv.tokenAuthStatus)
	}

	srv.autoRegister = srv.registerAutomatic
	srv.autoRegister = srv.finishRegistration(srv.autoRegister)
	if srv.opts.claudeFollow != nil {
		srv.opts.claudeFollow.install(srv.ctx, srv.opts, &srv.mu, &srv.workers, &srv.stopping, srv.bindings, srv.prepare, srv.autoRegister, srv.attached, srv.ensureUpstream)
	}
	if srv.opts.claudeFollow != nil {
		srv.opts.claudeFollow.installUnregistered(srv.opts, srv.autoAttach)
	}
	if srv.opts.configure != nil {
		srv.opts.configure(srv.server)
	}
	if srv.opts.enabled && srv.opts.tokenMode && srv.opts.identityFailure == nil {
		srv.server.AddReceivingMiddleware(srv.tokenModeMiddleware)
	}
	hideListedTool(srv.server, "ingress_hook", func(context.Context) bool { return srv.opts.harness == "claude" || srv.opts.harness == "kimi" })
	mcp.AddTool(srv.server, &mcp.Tool{Name: "ingress_hook", Description: "Internal Codex session lifecycle hook; never call manually or poll. Configure synchronous MCP hooks; see xbroker_docs setup/delivery."}, srv.ingressHook)
	addCompatibilityMiddleware(srv.server, srv.currentUpstream)
	// SDK session shutdown is graceful: it waits for incoming host calls.
	// Connect's context alone does not cancel those calls. Tie their upstream
	// HTTP work to this client's lifetime as well, before waiting for Close.
	srv.server.AddReceivingMiddleware(srv.requestLifetimeMiddleware)
	installKimiStatusMiddleware(srv.server, srv.ctx, srv.upstream, srv.opts)
	addToolProfileMiddleware(srv.server)
	return srv.server, srv.shutdown
}

// Registration locks are held for the process lifetime. Once this server
// stops, the stopping mark (set under mu, the same lock every worker
// registration takes) closes the window for new receivers and register
// handlers, every in-flight worker is joined so no state write survives,
// and the locks are released for a replacement client on the same PC.
func (srv *ingressServer) shutdown() {
	<-srv.ctx.Done()
	srv.mu.Lock()
	srv.stopping = true
	srv.mu.Unlock()
	srv.workers.Wait()
	if srv.opts.activityPool != nil {
		srv.opts.activityPool.wg.Wait()
	}
	srv.shutdownOnce.Do(func() {
		if srv.opts.state != nil {
			_ = srv.opts.state.FlushDiagnostics()
		}
		var dropped []*ingressBinding
		srv.mu.Lock()
		for _, b := range srv.bindings {
			if b.followMessage != nil {
				b.followMessage = nil
				dropped = append(dropped, b)
			}
			if b.unlock != nil {
				b.unlock()
				b.unlock = nil
			}
			if b.upstream != nil {
				closeUpstream(b.upstream)
				b.upstream = nil
			}
		}
		srv.mu.Unlock()
		for _, b := range dropped {
			srv.opts.diagnose(b, "follow-message-dropped", "sink", "")
		}
	})
}

func (srv *ingressServer) upstreamFor(b *ingressBinding) Upstream {
	if b != nil && b.upstream != nil {
		return b.upstream
	}
	return srv.upstream
}

func (srv *ingressServer) currentUpstream(callCtx context.Context, req *mcp.CallToolRequest) Upstream {
	srv.mu.Lock()
	bound := len(srv.bindings) != 0
	srv.mu.Unlock()
	if !bound {
		return srv.upstream
	}
	id, err := srv.identify(callCtx, req)
	if err != nil {
		return srv.upstream
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.upstreamFor(srv.bindings[id.SessionID])
}

func (srv *ingressServer) startPrune() {
	srv.pruneOnce.Do(func() {
		if srv.opts.state == nil {
			return
		}
		srv.mu.Lock()
		if srv.stopping {
			srv.mu.Unlock()
			return
		}
		srv.workers.Add(1)
		srv.mu.Unlock()
		go func() {
			defer srv.workers.Done()
			cleanupCodexYields(srv.opts.state.Dir(), time.Now())
			counts, err := srv.opts.state.Prune(time.Now())
			if err != nil {
				fmt.Fprintf(os.Stderr, "client-state-prune sessions=%d registrations=%d diagnostics=%d steer-attempts=%d skipped=%d errors=%d\n", counts.Sessions, counts.Registrations, counts.Diagnostics, counts.SteerAttempts, counts.Skipped, counts.Errors)
			}
		}()
	})
}
