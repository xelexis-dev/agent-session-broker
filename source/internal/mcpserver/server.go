package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type Backend interface {
	ListSessions(context.Context) ([]broker.Session, error)
	GetSession(context.Context, string) (broker.Session, error)
	SendMessage(context.Context, broker.Message) (broker.SendResult, error)
	WaitForMessage(context.Context, string) (broker.Message, error)
	StopSession(context.Context, string) error
}

type MemoryBackend interface {
	Put(context.Context, memory.PutInput) (memory.Node, error)
	Get(context.Context, string) (memory.GetResult, error)
	Search(context.Context, memory.SearchInput) (memory.SearchResult, error)
}

type nameInput struct {
	Name string `json:"name" jsonschema:"Session.Name communication address or session ID"`
}

type memoryGetInput struct {
	ID string `json:"id" jsonschema:"memory node id"`
}

type waitInput = protocol.IngressRequest

type sendInput struct {
	ExpectRegistrationID *string `json:"expectRegistrationId,omitempty" jsonschema:"Expected recipient registration ID for replies or known peers; mismatch refuses without enqueueing"`
	From                 string  `json:"from" jsonschema:"registered live sender Session.Name or session ID in this project; registration is required but is not caller authentication"`
	To                   string  `json:"to" jsonschema:"recipient Session.Name communication address or session ID"`
	Body                 string  `json:"body"`
}

type okOutput struct {
	OK bool `json:"ok"`
}

// Tool types are fixed; keep this cache out of the per-connection client options.
var serverSchemaCache = mcp.NewSchemaCache()

func New(backend Backend, memories MemoryBackend, version ...string) *mcp.Server {
	return newWithCompatibility(backend, memories, nil, version...)
}

// A non-nil policy is used by isolated compatibility tests.
func newWithCompatibility(backend Backend, memories MemoryBackend, policy *serverCompatibility, version ...string) *mcp.Server {

	if memories == nil {
		panic("mcpserver.New requires a memory backend")
	}
	options := mcpconfig.ServerOptions()
	options.SchemaCache = serverSchemaCache
	options.Instructions += " Reply to x-broker messages with x-broker send_message."
	options.Capabilities.Experimental = map[string]any{"recommendedClientVersion": mcpconfig.RecommendedClientVersion, protocol.MessageSentAtCapability: true, protocol.ReceiverHolderCapability: true, protocol.RecipientExpectationCapability: true}
	if _, ok := backend.(broker.DurableRegistrar); ok {
		options.Capabilities.Experimental[protocol.MessageAckCapability] = true
	}
	if a, ok := backend.(interface{ AttachmentsAvailable() bool }); ok && a.AttachmentsAvailable() {
		options.Capabilities.Experimental[protocol.AttachmentCapability] = true
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "agent-session-broker", Version: mcpconfig.Version(version...)}, options)
	s.AddReceivingMiddleware(scopeMiddleware)
	s.AddReceivingMiddleware(projectCapabilityMiddleware)

	ownVersion := mcpconfig.Version(version...)
	if policy == nil {
		selected, _ := protocol.ServerPolicy(ownVersion)
		policy = &selected
	}
	s.AddReceivingMiddleware(compatibilityMiddleware(ownVersion, *policy))
	memories = scopedMemory{memories}
	_, remoteService := backend.(*broker.RemoteService)
	_, durableBackend := backend.(broker.DurableRegistrar)
	describe := func(tool *mcp.Tool) *mcp.Tool {
		if remoteService || durableBackend {
			if description := remoteToolDescriptions[tool.Name]; description != "" {
				tool.Description = description
			}
		}
		return tool
	}
	if rebinder, ok := backend.(interface {
		RebindSession(context.Context, protocol.RebindRequest) (protocol.RebindResult, error)
	}); ok {
		mcp.AddTool(s, &mcp.Tool{Name: "rebind_session", Description: "Internal explicit approved conversation handover; never creates a registration."}, func(ctx context.Context, _ *mcp.CallToolRequest, in protocol.RebindRequest) (*mcp.CallToolResult, protocol.RebindResult, error) {
			result, err := rebinder.RebindSession(ctx, in)
			var refusal protocol.HandoverRefusal
			if errors.As(err, &refusal) {
				return nil, protocol.RebindResult{Reason: protocol.SafeHandoverReason(refusal.Reason), HolderChallenge: refusal.HolderChallenge}, nil
			}
			return nil, result, err
		})
	}
	if remoteService || durableBackend {
		addTokenInfoTool(s)
		addBugReportTool(s, backend, ownVersion)
		addBugReportManagementTools(s, backend)
		// Keep the registered handler for old embedded catalogs, but stop
		// advertising an operation this deployment cannot perform.
		s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				result, err := next(ctx, method, req)
				if method != "tools/list" || err != nil {
					return result, err
				}
				listed, ok := result.(*mcp.ListToolsResult)
				if !ok {
					return result, err
				}
				out := *listed
				out.Tools = make([]*mcp.Tool, 0, len(listed.Tools))
				for _, tool := range listed.Tools {
					if tool.Name != "rebind_session" && tool.Name != access.TokenInfoTool {
						out.Tools = append(out.Tools, legacyAttachmentTool(tool))
					}
				}
				return &out, nil
			}
		})
	}
	// The old project-token client exposes a strict embedded Session schema.
	// Project only those public Session results; internal negotiation results,
	// list_sessions and the panel keep the full snapshot.
	sessionOutput := func(ctx context.Context, value broker.Session) broker.Session {
		if gate, ok := backend.(interface{ SessionActivityOutputAllowed(context.Context) bool }); ok && !gate.SessionActivityOutputAllowed(ctx) {
			value.Activity = nil
		}
		return value
	}
	if ingress, ok := backend.(interface {
		ReportIngressFailure(context.Context, string, string) error
	}); ok {
		type failureInput struct {
			SessionID      string `json:"sessionId,omitempty"`
			RegistrationID string `json:"registrationId,omitempty"`
			ChannelID      string `json:"channelId"`
		}
		mcp.AddTool(s, describe(&mcp.Tool{Name: "report_ingress_failure", Description: "Internal local receiver failure report, bound to the current ingress lease. Not a model messaging tool."}), func(ctx context.Context, _ *mcp.CallToolRequest, in failureInput) (*mcp.CallToolResult, okOutput, error) {
			id := in.SessionID
			if in.RegistrationID != "" {
				id = in.RegistrationID
			}
			err := ingress.ReportIngressFailure(ctx, id, in.ChannelID)
			return nil, okOutput{OK: err == nil}, err
		})
	}
	if remoteService || durableBackend {
		mcp.AddTool(s, &mcp.Tool{Name: "retire_session", Description: "Retire another session: revoke its credential and free its name. Self-retire is forbidden. Use exactly one registrationId or orphan name; requires session:manage/owner rights. Suspended/unused targets are refused. Check names after uncertain results; cleanup-required retries by ID. See xbroker_docs names."}, func(ctx context.Context, _ *mcp.CallToolRequest, in protocol.RetireRequest) (*mcp.CallToolResult, any, error) {
			retire, ok := backend.(interface {
				RetireSession(context.Context, protocol.RetireRequest) (protocol.RetireResult, error)
			})
			if !ok {
				return &mcp.CallToolResult{IsError: true}, protocol.RejectRetire("forbidden"), nil
			}
			result, err := retire.RetireSession(ctx, in)
			if err != nil {
				var rejection protocol.RetireRejection
				if errors.As(err, &rejection) {
					return &mcp.CallToolResult{IsError: true}, rejection, nil
				}
				return nil, nil, err
			}
			return nil, result, nil
		})
	}
	if renamer, ok := backend.(interface {
		RenameSession(context.Context, broker.RenameRequest) (broker.Session, error)
	}); ok {
		mcp.AddTool(s, &mcp.Tool{Name: "rename_session", Description: "Rename this connected session immediately. The local client supplies its registration and active lease. Duplicate or invalid nicknames are rejected; registration, delivery and queued messages are preserved. Use the returned name for subsequent messages."}, func(ctx context.Context, _ *mcp.CallToolRequest, in broker.RenameRequest) (*mcp.CallToolResult, any, error) {
			v, err := renamer.RenameSession(ctx, in)
			var rejection protocol.RenameRejection
			if errors.As(err, &rejection) {
				return &mcp.CallToolResult{IsError: true}, rejection, nil
			}
			return nil, sessionOutput(ctx, v), err
		})
	}
	if durable, ok := backend.(broker.DurableRegistrar); ok {
		mcp.AddTool(s, describe(&mcp.Tool{Name: "register_session", Description: "Internal explicit registration used by the local MCP client for attach_session. Creates, revives or renames the durable registration owned by (harness, session UUID, client id) after verifying the client-held resume secret. A requested name held by another session yields a UUID nickname and a warning. The result code, not a tool error, reports owned, invalid-token, storage or invalid. Not a model messaging tool."}), func(ctx context.Context, _ *mcp.CallToolRequest, in broker.RegisterRequest) (*mcp.CallToolResult, broker.RegisterResult, error) {
			v, e := durable.RegisterSession(ctx, in)
			return nil, credentialScopesResult(ctx, v, e), e
		})
		mcp.AddTool(s, describe(&mcp.Tool{Name: "resume_session", Description: "Internal automatic recovery used by the local MCP client after a network or server restart. Restores an existing durable registration with a fresh lease when the resume secret matches; it never creates a registration and is rejected with code detached after stop_session. Not a model messaging tool."}), func(ctx context.Context, _ *mcp.CallToolRequest, in broker.ResumeRequest) (*mcp.CallToolResult, broker.RegisterResult, error) {
			v, e := durable.ResumeSession(ctx, in)
			return nil, credentialScopesResult(ctx, v, e), e
		})
		mcp.AddTool(s, describe(&mcp.Tool{Name: "renew_lease", InputSchema: renewInputSchema(), Description: "Internal heartbeat used by the local MCP client. Renews the current ingress lease and returns any pending identity probe. Not a model messaging tool."}), func(ctx context.Context, _ *mcp.CallToolRequest, in broker.RenewRequest) (*mcp.CallToolResult, broker.RegisterResult, error) {
			v, e := durable.RenewLease(ctx, in)
			v = credentialScopesResult(ctx, v, e)
			if e == nil && v.Code == "" {
				return &mcp.CallToolResult{Content: []mcp.Content{}}, v, nil
			}
			return nil, v, e
		})
		mcp.AddTool(s, describe(&mcp.Tool{Name: "probe_response", Description: "Internal answer to a pending identity probe, produced by the local MCP client without model involvement. A mismatch marks the registration missing without removing it; a stale probe id, revision or lease is ignored with code stale-probe. Not a model messaging tool."}), func(ctx context.Context, _ *mcp.CallToolRequest, in broker.ProbeAnswer) (*mcp.CallToolResult, broker.ProbeResult, error) {
			v, e := durable.AnswerProbe(ctx, in)
			return nil, v, e
		})
	}
	if external, ok := backend.(interface {
		AttachSession(context.Context, broker.AttachRequest) (broker.Session, error)
	}); ok {
		mcp.AddTool(s, describe(&mcp.Tool{Name: "attach_session", Description: "Connect an existing Claude or Codex using exact sessionId, PID and absolute cwd. For Codex set harness=codex and name; local session metadata and the live process creation identity are checked. Claude accepts an optional communication name (default: existing registration or CLI name); neither harness is spawned or restarted. External Codex uses its native CLI input queue. External Claude requires a ready local ingress bridge. Neither external process is restarted. stop_session only detaches externallyManaged sessions and discards pending messages. Rejections contain an allowlisted code and a safe user guide."}), func(ctx context.Context, _ *mcp.CallToolRequest, in broker.AttachRequest) (*mcp.CallToolResult, any, error) {
			v, e := external.AttachSession(ctx, in)
			if e != nil {
				safe := safeAttachRejection(e)
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: safe.Error()}}}, safe, nil
			}
			return nil, v, e
		})
	}
	mcp.AddTool(s, describe(&mcp.Tool{Name: "list_sessions", Description: "List Claude and Codex sessions with normalized liveness. A stable Session.Name is the communication address across a rotating Claude ID. An idle Claude wake can start one metered Claude turn: it stops the exact old generation and resumes the same conversation with a current sessionId. Refresh identity through the returned sessionId or list_sessions: cached old IDs return session not found and are never forwarded. accepted means the queue accepted the body; confirmed means the new identity committed and one wait claimed it, not that work completed. failed keeps the old generation authoritative with its body queued; degraded blocks new delivery until exact cleanup or inspection. Bodies do not survive broker restart, which starts no retry. The stable name is CLI metadata and is supplied in separate connection instructions; the model wake prompt is constant with no interpolation."}), func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		v, err := backend.ListSessions(ctx)
		if err != nil {
			return nil, nil, err
		}
		if v == nil {
			v = []broker.Session{}
		}
		for i := range v {
			if v[i].Harness == "kimi" {
				v[i].AutomaticDeliveryReady = false
			}
		}
		data, err := json.Marshal(v)
		if err != nil {
			return nil, nil, err
		}
		// Preserve array text for clients without SEP-2106 structured output support.
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
	})
	mcp.AddTool(s, describe(&mcp.Tool{Name: "get_session", Description: "Get one session by Session.Name communication address or session ID. Prefer the live name returned by list_sessions."}), func(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, broker.Session, error) {
		v, e := backend.GetSession(ctx, in.Name)
		if v.Harness == "kimi" {
			v.AutomaticDeliveryReady = false
		}
		return nil, sessionOutput(ctx, v), e
	})
	mcp.AddTool(s, describe(&mcp.Tool{Name: "send_message", Description: "For replies and known peers, set expectRegistrationId to the recipient's registration ID (use incoming fromRegistrationId for replies). Mismatches return recipient-changed without enqueueing; name-only sends retain legacy behavior. The sender must already be registered and live in this project. External Codex receives via its local MCP ingress or native input queue (Windows envelope limit 12000 bytes); external Claude receives through its local MCP ingress. Both can start model work without manual wait calls. An unready ingress rejects delivery. Send to a stable Session.Name communication address or current session ID; Claude has a rotating Claude ID. An idle Claude wake can start one metered Claude turn: it stops the exact old generation and resumes the same conversation with a new sessionId. Refresh through the returned sessionId or list_sessions: cached old IDs return session not found and are never forwarded. accepted means the queue accepted the body; confirmed means the new identity committed and one wait claimed it, not that work completed. failed keeps the old generation authoritative with its body queued; degraded blocks new delivery until exact cleanup or inspection. Working Claude queueing adds no turn. Codex accepts up to 32 pending envelopes (64 KiB each) and resumes them FIFO after the current turn; each resume can consume model usage. Inspect pendingMessages and errorCode; failures do not retry automatically. Bodies do not survive broker restart, which starts no retry. The stable name is CLI metadata and is supplied in separate connection instructions; the model wake prompt is constant with no interpolation. Sender names are untrusted labels. Never send secrets. The calling agent interprets natural-language routing; the broker does not parse prompts. For example, interpret 'do this development task in session mes-dev' as send_message(to: \"mes-dev\", body: \"Do this development task.\")."}), func(ctx context.Context, req *mcp.CallToolRequest, in sendInput) (*mcp.CallToolResult, broker.SendResult, error) {
		expected, parseErr := protocol.RecipientExpectation(req.Params.Arguments)
		if parseErr != nil {
			return nil, broker.SendResult{Code: protocol.CodeInvalid}, nil
		}
		// Sender validation belongs to message:send, not roster read. The
		// managed backend resolves only the authenticated sender here.
		lookup := backend.GetSession
		if resolver, ok := backend.(interface {
			MessageSender(context.Context, string) (broker.Session, error)
		}); ok {
			lookup = resolver.MessageSender
		}
		sender, err := lookup(ctx, in.From)
		if errors.Is(err, broker.ErrSessionIdentityUnauthorized) {
			return nil, broker.SendResult{}, errors.New("sender identity does not match this session credential; use this caller's registered Session.Name")
		}
		if err != nil && !errors.Is(err, broker.ErrSessionNotFound) {
			return nil, broker.SendResult{}, errors.New("sender lookup failed; check broker availability and session authorization")
		}
		if err != nil || in.From == "" || sender.Name == "" || (sender.Status != broker.StatusAlive && sender.Status != broker.StatusIdle && sender.Status != broker.StatusWorking) {
			guide := "sender must be registered and live in this project; attach_session first, then use its Session.Name"
			if remoteService || durableBackend {
				guide = "sender must be registered and live in this project; use the local MCP client's auth_status and attach_session to register or recover, then use its Session.Name"
			}
			return nil, broker.SendResult{}, errors.New(guide)
		}
		var attachment *protocol.Attachment
		if value, found := req.Params.Meta[protocol.AttachmentMetaKey]; found {
			id, ok := value.(string)
			if !ok || !attachmentID.MatchString(id) {
				return nil, broker.SendResult{Code: protocol.CodeInvalid}, nil
			}
			attachment = &protocol.Attachment{ID: id}
		}
		value, e := backend.SendMessage(ctx, broker.Message{ExpectRegistrationID: expected, From: sender.Name, To: in.To, Body: in.Body, Attachment: attachment})
		return nil, value, e
	})
	mcp.AddTool(s, describe(&mcp.Tool{Name: "wait_for_message", Description: "Block until messages are available for a Claude or external Codex Session.Name communication address or session ID. The local ingress bridge calls this automatically; do not run a competing manual receiver. Direct Codex native queue messages do not appear here. A single delivery returns the compatible from/to/body envelope. One idle wake atomically drains all earlier pending messages plus the wake message as batch.messages in FIFO order; handle every envelope in that array."}), func(ctx context.Context, _ *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, broker.Message, error) {
		if receiver, ok := backend.(interface {
			WaitForIngressAck(context.Context, protocol.IngressRequest) (broker.Message, error)
		}); ok {
			v, e := receiver.WaitForIngressAck(ctx, in)
			return nil, v, e
		}
		if remote, ok := backend.(interface {
			WaitForIngress(context.Context, string, string) (broker.Message, error)
		}); ok {
			v, e := remote.WaitForIngress(ctx, in.Recipient, in.ChannelID)
			return nil, v, e
		}
		v, e := backend.WaitForMessage(ctx, in.Recipient)
		return nil, v, e
	})
	mcp.AddTool(s, describe(&mcp.Tool{Name: "stop_session", Description: "Detach this session selected by Session.Name communication address or session ID. With session:manage, another session may also be detached when allowed by the token owner’s current project management permissions. Externally managed processes are never stopped. The user must request attach_session before a detached conversation reconnects."}), func(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, okOutput, error) {
		e := backend.StopSession(ctx, in.Name)
		return nil, okOutput{OK: e == nil}, e
	})
	manager, managementSupported := backend.(interface {
		SuspendSession(context.Context, string) error
		UnsuspendSession(context.Context, string) error
	})
	if managementSupported || remoteService || durableBackend {
		mcp.AddTool(s, describe(&mcp.Tool{Name: "suspend_session", Description: "Suspend another session permitted by the token owner's current project management rights. Requires session:manage. Discards queued messages and cancels in-flight requests. Self suspension is denied; no process is terminated."}), func(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, okOutput, error) {
			if !managementSupported {
				return nil, okOutput{}, errors.New("session management is not supported by this backend")
			}
			err := manager.SuspendSession(ctx, in.Name)
			return nil, okOutput{OK: err == nil}, err
		})
		mcp.AddTool(s, describe(&mcp.Tool{Name: "unsuspend_session", Description: "Lift suspension of another manageable session. Requires session:manage and current project management rights. Self unsuspension is denied. The target reconnects on its next lease; no process is started."}), func(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, okOutput, error) {
			if !managementSupported {
				return nil, okOutput{}, errors.New("session management is not supported by this backend")
			}
			err := manager.UnsuspendSession(ctx, in.Name)
			return nil, okOutput{OK: err == nil}, err
		})
	}
	mcp.AddTool(s, describe(&mcp.Tool{Name: "memory_put", Description: "Create immutable project memory. Never store tokens, passwords, keys or other secrets."}), func(ctx context.Context, _ *mcp.CallToolRequest, in memory.PutInput) (*mcp.CallToolResult, memory.Node, error) {
		v, e := memories.Put(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(s, describe(&mcp.Tool{Name: "memory_get", Description: "Read immutable project memory and graph links."}), func(ctx context.Context, _ *mcp.CallToolRequest, in memoryGetInput) (*mcp.CallToolResult, memory.GetResult, error) {
		v, e := memories.Get(ctx, in.ID)
		return nil, v, e
	})
	mcp.AddTool(s, describe(&mcp.Tool{Name: "memory_search", Description: "Search immutable project memory."}), func(ctx context.Context, _ *mcp.CallToolRequest, in memory.SearchInput) (*mcp.CallToolResult, memory.SearchResult, error) {
		v, e := memories.Search(ctx, in)
		return nil, v, e
	})
	return s
}

func Handler(server *mcp.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true}))
	mux.HandleFunc("/healthz", healthHandler)
	return mux
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
