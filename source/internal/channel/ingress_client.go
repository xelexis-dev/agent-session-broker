package channel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/codex"
	"github.com/xelexis-dev/agent-session-broker/source/internal/localactivity"
	"github.com/xelexis-dev/agent-session-broker/source/internal/nativeinbox"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type ingressSinkFactory func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error)
type ingressBinding struct {
	claimThread            string
	claimConfirmed         bool
	threadConfirmed        bool
	messageAck             *messageAckState
	followAck              func(context.Context) error
	holderWaiting          bool
	holderWaitAttempt      int
	retired                bool
	selfStopped            bool
	temporary              bool
	used                   bool
	manualSource           string
	followMessage          *broker.Message
	followStopped          bool
	followRestoreAt        time.Time
	ingressReady           bool // last successful broker readiness; guarded by mu
	ingressReadyChannel    string
	ingressReadyGeneration uint64
	resumeSuspended        bool // last definitive lease/resume result; guarded by mu
	mu                     *sync.Mutex
	registerMu             sync.Mutex
	attachmentMu           sync.Mutex
	// pendingResume is protected by registerMu. A canceled ordinary tool call
	// may have applied its resume, so the next attempt must replay it exactly.
	pendingResume      broker.ResumeRequest
	resumeStopReason   string // guarded by mu; sanitized broker outcome
	identity           broker.AttachRequest
	sink               func(context.Context, broker.Message) error
	nativeDeferred     bool
	sinkPrepared       bool
	receiveUnavailable bool
	receiveReason      string
	receiveGate        string // "follow" owns only the follow reconnect gate; guarded by mu
	router             *codexIngress
	started            bool
	initialContinuing  bool
	failed             bool
	generation         uint64
	activityEnabled    bool
	runtimeInfoEnabled bool
	cancel             context.CancelFunc
	done               chan struct{}
	durable            *durableRegistration
	unlock             func()
	// upstream is this conversation's own broker connection in project-token
	// mode; it carries only this conversation's session credential.
	upstream Upstream
	// upstreamCredential is the credential upstream was built with.
	upstreamCredential string
}

// unavailableUpstream stands in for the shared upstream in project-token
// mode, where every broker call must go through a conversation's own upstream.
type unavailableUpstream struct{}

func (unavailableUpstream) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return nil, errors.New("no session credential is available for this request")
}

func closeUpstream(u Upstream) {
	if closer, ok := u.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// deviceRequest addresses the automatic registration path at one known
// conversation, the way a harness call carrying thread metadata would.
func deviceRequest(sessionID string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{"threadId": sessionID}}}
}

func localIngressSink(identity broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
	if identity.Harness == "claude" {
		p, err := nativeinbox.FromEnvironment(identity.SessionID)
		if err != nil {
			return nil, nil, err
		}
		return p.Post, nil, nil
	}
	// Preserve the exact executable/process generation; no shell wrapper or remote server.
	adapter := codex.NewAdapter()
	identity.Name = "ingress-probe"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, key, err := adapter.InspectExternal(ctx, identity)
	if err != nil {
		return nil, nil, err
	}
	r := newCodexIngress(func(ctx context.Context, m broker.Message) error {
		target := identity
		target.Name = m.To
		return adapter.PushExternal(ctx, target, key, m)
	})
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
		home = filepath.Join(home, ".codex")
	}
	config := localactivity.Config{Binding: localactivity.Binding{Harness: "codex", SessionID: identity.SessionID}, Home: home, CWD: identity.CWD, PID: identity.PID}
	r.probe = func(ctx context.Context, turn string) bool { return localactivity.CodexTurnEnded(ctx, config, turn) }
	r.steer = &codexSteerer{dial: bindCodexControl(identity.PID), thread: identity.SessionID, delay: 10 * time.Second, gate: func() string { return "ack-required" }}
	return r.deliver, r, nil
}

func ServeIngress(ctx context.Context, transport mcp.Transport, upstream Upstream, tools []*mcp.Tool, identify func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return newIngressServer(ctx, upstream, tools, identify, localIngressSink).Run(ctx, transport)
}

// ServeDurableIngress is ServeIngress for brokers with durable registrations:
// the client owns a resume secret per session, reconnects automatically after
// a server restart and answers identity probes without the model. It returns
// only after every receiver has stopped and the registration locks are released.
func ServeDurableIngress(ctx context.Context, transport mcp.Transport, upstream Upstream, tools []*mcp.Tool, identify func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error), opts durableOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if opts.hookWrites == nil {
		opts.hookWrites = &hookWriteCompletions{}
	}
	transport = hookWriteTransport{Transport: transport, completions: opts.hookWrites}
	factory := localIngressSink
	if opts.kimi != nil {
		factory = func(id broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return opts.kimi.queue.deliver, nil, nil
		}
	}
	if opts.claudeInboxFactory != nil {
		factory = func(id broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			if id.Harness == "claude" {
				return opts.claudeInboxFactory(id)
			}
			return localIngressSink(id)
		}
	}
	server, shutdown := newIngressServerWithOptions(ctx, upstream, tools, identify, factory, opts)
	err := server.Run(ctx, transport)
	cancel()
	shutdown()
	return err
}

func newIngressServer(ctx context.Context, upstream Upstream, tools []*mcp.Tool, identify func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error), factory ingressSinkFactory) *mcp.Server {
	server, _ := newIngressServerWithOptions(ctx, upstream, tools, identify, factory, durableOptions{})
	return server
}

func reportIngressFailure(upstream Upstream, id broker.AttachRequest, registrationID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	report := map[string]string{"sessionId": id.SessionID, "channelId": id.ChannelID}
	if registrationID != "" {
		report["registrationId"] = registrationID
	}
	request := &mcp.CallToolParams{Name: "report_ingress_failure", Arguments: report}
	_, err := upstream.CallTool(ctx, request)
	if errors.Is(err, mcp.ErrConnectionClosed) {
		// Receiver cancellation can leave a closed cached session; its first call discards it.
		_, _ = upstream.CallTool(ctx, request)
	}
}

func receiveIngress(ctx context.Context, upstream Upstream, id broker.AttachRequest, sink func(context.Context, broker.Message) error) {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		reportIngressFailure(upstream, id, "")
	}()
	results := make(chan error, 2)
	workers.Add(2)
	go func() {
		defer workers.Done()
		results <- maintainRegistration(ctx, upstream, id, 5*time.Second, 5*time.Second)
	}()
	go func() {
		defer workers.Done()
		for {
			var message broker.Message
			// The stream belongs to this receiver, not a periodic poll. Cancelling
			// each idle wait sends a notification to the stateless HTTP server and
			// can close the shared MCP connection, including unrelated tool calls.
			err := call(ctx, upstream, "wait_for_message", map[string]string{"recipient": id.Name, "channelId": id.ChannelID}, &message)
			if err != nil {
				results <- err
				return
			}
			if message.To != id.Name {
				results <- errors.New("ingress recipient mismatch")
				return
			}
			if err := sink(ctx, message); err != nil {
				results <- err
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-results:
	}
}
