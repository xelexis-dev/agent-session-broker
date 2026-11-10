package channel

import (
	"context"
	"runtime"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const versionInstructions = " Inspect clientVersion/serverVersion with auth_status; unknown means no upstream connection."

func (u *brokerUpstream) versionInfo() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.serverVersion
}

func (u *receiveUpstream) versionInfo() string { return upstreamVersion(u.Upstream) }

func upstreamVersion(upstream Upstream) string {
	if source, ok := upstream.(interface{ versionInfo() string }); ok {
		return source.versionInfo()
	}
	if session, ok := upstream.(*mcp.ClientSession); ok && session.InitializeResult() != nil && session.InitializeResult().ServerInfo != nil {
		return session.InitializeResult().ServerInfo.Version
	}
	return ""
}

type launchHintKey struct{}

func launcherHint(platform string, ancestors []int, harnessPID int) string {
	if platform == "windows" || len(ancestors) == 0 || ancestors[0] <= 0 || harnessPID <= 0 || ancestors[0] == harnessPID {
		return ""
	}
	return "a launcher process stays between the harness and this client (for example npx); see xbroker_docs topic=setup"
}

func authVersions(ctx context.Context, upstream Upstream, out authStatus, host ...string) authStatus {
	out.ToolProfile = clientToolProfile(ctx)
	out.ToolProfileNote = "hidden from the list only; authorization unchanged"
	out.LaunchHint, _ = ctx.Value(launchHintKey{}).(string)
	if out.ScopeSource == "" {
		out.ScopeSource = "unknown"
	}
	if actual := projectUpstream(upstream); actual != nil && out.Status == authAuthorized && !upstreamRejected(upstream) && projectTerminalUpstream(upstream) == nil {
		if failure := actual.projectFailure.Load(); failure != nil && !failure.terminal && (failure.code == "project_disabled" || failure.code == "project_deleting") {
			out.Message = failure.Error()
		}
	}
	out.ClientVersion = mcpconfig.ContextVersion(ctx)
	out.Update = nil
	recommended := upstreamRecommendedClient(upstream)
	if order, valid := protocol.CompareVersions(out.ClientVersion, recommended); valid && order < 0 {
		harness := ""
		if len(host) > 0 {
			harness = host[0]
		}
		out.Update = &clientUpdate{Available: true, Current: out.ClientVersion, Recommended: recommended, Steps: updateSteps(harness, runtime.GOOS)}
	}

	out.ServerVersion = strings.TrimSpace(upstreamVersion(upstream))
	if out.ServerVersion == "" {
		out.ServerVersion = "unknown"
	}
	out.ServerVersionKnown = out.ServerVersion != "unknown" && out.ServerVersion != "0.1.0"
	state := assessCompatibility(out.ClientVersion, out.ServerVersion)
	if source, ok := upstream.(interface{ compatibilityInfo() compatibilityState }); ok {
		state = source.compatibilityInfo()
	}
	out.Compatible, out.RequiredClient, out.RequiredServer = state.Compatible, state.RequiredClient, state.RequiredServer
	if !state.Compatible {
		out.Message = state.Message
	}
	return out
}

// Read the exact conversation connection; no server-version fallback or network query.
func upstreamRecommendedClient(upstream Upstream) string {
	switch u := upstream.(type) {
	case *receiveUpstream:
		return upstreamRecommendedClient(u.Upstream)
	case *brokerUpstream:
		u.mu.Lock()
		session := u.current
		u.mu.Unlock()
		if session != nil {
			return upstreamRecommendedClient(session)
		}
	case *mcp.ClientSession:
		if init := u.InitializeResult(); init != nil {
			recommended, _ := init.Capabilities.Experimental["recommendedClientVersion"].(string)
			return recommended
		}
	}
	return ""
}
func updateSteps(host, platform string) string {
	steps := "Update the configured npm version and reconnect using the original host mode and flags."
	switch host {
	case "claude":
		steps = "Update the configured npm version, then /mcp > x-broker > Reconnect; confirm clientVersion and the refreshed tool list."
	case "codex":
		steps = "Update the configured npm version and reopen using the original host mode and flags; resume exec as exec. Reopening a daemon-based TUI alone does not reload MCP."
	case "kimi":
		steps = "Update the configured npm version and restart Kimi; Kimi upgrade/reconnect behavior is unmeasured."
	}
	if platform == "windows" {
		steps += " Windows: launcher 0.17.3+ supports npm upgrade while sessions stay connected; older launchers require stopping asb-client before upgrading."
	}
	return steps
}
