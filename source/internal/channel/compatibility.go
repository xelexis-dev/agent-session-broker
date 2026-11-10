package channel

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type compatibilityState struct {
	Compatible     bool
	RequiredClient string
	RequiredServer string
	Code           string
	Message        string
}
type compatibilityError struct{ message string }

func (e *compatibilityError) Error() string { return e.message }
func isCompatibilityError(err error) bool   { var e *compatibilityError; return errors.As(err, &e) }
func assessCompatibility(client, server string) compatibilityState {
	out := compatibilityState{Compatible: true}
	policy, ok := protocol.ClientPolicy(client)
	if ok {
		out.RequiredServer = policy.MinServer
	}
	reject := func(code, min string) compatibilityState {
		out.Compatible = false
		out.Code = code
		out.Message = protocol.CompatibilityGuidance(code, client, server, min)
		return out
	}
	if !ok {
		return reject("client-incompatible", "unknown")
	}
	if protocol.VersionLine(server) == "" {
		// Unversioned own server builds select the newest server policy. Apply the
		// same line when comparing a peer development build, without trusting or
		// displaying its arbitrary build label.
		development, ok := protocol.ServerPolicy(server)
		if !ok {
			return reject("server-incompatible", policy.MinServer)
		}
		server = development.Line + ".0"
	}

	if c, valid := protocol.CompareVersions(server, policy.MinServer); !valid || c < 0 {
		return reject("server-incompatible", policy.MinServer)
	}
	return out
}
func (u *brokerUpstream) compatibilityInfo() compatibilityState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.compatibility
}
func (u *receiveUpstream) compatibilityInfo() compatibilityState {
	return upstreamCompatibility(u.Upstream)
}
func upstreamCompatibility(u Upstream) compatibilityState {
	if source, ok := u.(interface{ compatibilityInfo() compatibilityState }); ok {
		return source.compatibilityInfo()
	}
	return compatibilityState{Compatible: true}
}
func compatibilityResult(state compatibilityState) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: state.Message}}}
}
func negotiatedCompatibility(client string, init *mcp.InitializeResult) compatibilityState {
	version := ""
	if init != nil && init.ServerInfo != nil {
		version = init.ServerInfo.Version
	}
	return assessCompatibility(client, version)
}

// Never retain or surface arbitrary server error text. A recognized prefix
// triggers degradation even if its fields are invalid, using local wording.
func gateCompatibility(result *mcp.CallToolResult, client, server string, prior compatibilityState) (compatibilityState, bool) {
	if result == nil || !result.IsError {
		return prior, false
	}
	for _, content := range result.Content {
		text, ok := content.(*mcp.TextContent)
		if !ok || !strings.HasPrefix(text.Text, "client-incompatible:") {
			continue
		}
		prior.Compatible = false
		prior.Code = "client-incompatible"
		prior.RequiredClient = ""
		prior.Message = protocol.CompatibilityGuidance("client-incompatible", client, server, "dev/unknown")
		if len(text.Text) > 512 {
			return prior, true
		}
		// Parse only the fixed header; canonical regeneration validates punctuation,
		// guidance and all fields before anything from the peer is kept.
		header, _, ok := strings.Cut(text.Text, ". Run ")
		if !ok {
			return prior, true
		}
		fields := strings.Split(header, "; ")
		if len(fields) != 3 {
			return prior, true
		}
		c, ok1 := strings.CutPrefix(fields[0], "client-incompatible: client ")
		s, ok2 := strings.CutPrefix(fields[1], "server ")
		min, ok3 := strings.CutPrefix(fields[2], "required minimum ")
		valid := func(v string) bool {
			_, ok := protocol.CompareVersions(v, v)
			return ok || v == "<=0.14" || v == "dev/unknown"
		}
		if ok1 && ok2 && ok3 && valid(c) && valid(s) && valid(min) && protocol.CompatibilityGuidance("client-incompatible", c, s, min) == text.Text {
			prior.Message = text.Text
			prior.RequiredClient = min
		}
		return prior, true
	}
	return prior, false
}
func localReadTool(name string) bool {
	return name == "auth_status" || name == "list_sessions" || name == "get_session" || name == "xbroker_docs"
}
func addCompatibilityMiddleware(server *mcp.Server, lookup func(context.Context, *mcp.CallToolRequest) Upstream) {
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				if r, ok := req.(*mcp.CallToolRequest); ok && !localReadTool(r.Params.Name) {
					if state := upstreamCompatibility(lookup(ctx, r)); !state.Compatible {
						return compatibilityResult(state), nil
					}
				}
			}
			return next(ctx, method, req)
		}
	})
}
