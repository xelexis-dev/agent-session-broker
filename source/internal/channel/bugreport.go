package channel

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"runtime"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport/clientdiag"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
)

type bugDiagnosticsKey struct{}

var diagnosticVersionPattern = regexp.MustCompile(`^[A-Za-z0-9.+_-]{1,80}$`)

func safeBugVersion(value string) string {
	if !diagnosticVersionPattern.MatchString(value) || strings.Contains(value, "asb_") {
		return "unknown"
	}
	return value
}
func baseBugDiagnostics(ctx context.Context, target Upstream) *clientdiag.ClientDiagnostics {
	return &clientdiag.ClientDiagnostics{ClientVersion: safeBugVersion(mcpconfig.ContextVersion(ctx)), ServerVersion: safeBugVersion(upstreamVersion(target)), OS: runtime.GOOS, Arch: runtime.GOARCH, AuthState: authUnauthorized}
}
func (o durableOptions) bugDiagnostics(ctx context.Context, b *ingressBinding, target Upstream) *clientdiag.ClientDiagnostics {
	out := baseBugDiagnostics(ctx, target)
	if b == nil {
		return out
	}
	b.mu.Lock()
	if b.identity.Harness == "claude" || b.identity.Harness == "codex" {
		out.Harness = b.identity.Harness
	}
	out.ReceiveReady = b.started && !b.failed && !b.receiveUnavailable && !b.retired && !receiveDenied(target) && !o.ingressPending()
	d := b.durable
	key := ""
	if d != nil {
		key = d.key
	}
	b.mu.Unlock()
	if d != nil && d.session != nil {
		d.session.mu.Lock()
		switch d.session.status {
		case authUnauthorized, authPending, authAuthorized, authDenied, authExpired, authRejected, authFailed:
			out.AuthState = d.session.status
		}
		d.session.mu.Unlock()
	}
	if o.claudeFollow != nil {
		summary := o.claudeFollow.summary()
		switch summary {
		case "followed: handover", "followed: resumed", "follow-skipped: parent-changed", "follow-skipped: unavailable", "follow-skipped: refused", "follow-skipped: detached", "follow-skipped: suspended", "follow-skipped: not-found", "follow-skipped: still-receiving", "follow-skipped: secret-mismatch", "follow-skipped: client-mismatch", "follow-skipped: owner-mismatch", "follow-skipped: unsupported-mode", "follow-skipped: identity-conflict":
			out.LastFollow = summary
		}
	}
	if o.state != nil && key != "" {
		diagnostics, err := o.state.LoadDiagnostics(key)
		if err == nil {
			diagnostics = clientstate.SafeDiagnostics(diagnostics)
			start := max(0, len(diagnostics.Events)-20)
			for _, event := range diagnostics.Events[start:] {
				// The display projection hides unknown codes and extension fields. Deliberately
				// copy no IDs, free text, credentials or diagnostic summary payload.
				reason := event.ErrorClass
				if reason == "" {
					reason = event.Reason
				}
				out.Events = append(out.Events, clientdiag.DiagnosticEvent{At: event.At, Event: event.Event, ReasonClass: reason})
			}
		}
	}
	return out
}

func bugReportArguments(ctx context.Context, req *mcp.CallToolRequest, target Upstream) (json.RawMessage, error) {
	var input map[string]json.RawMessage
	if json.Unmarshal(req.Params.Arguments, &input) != nil || input == nil {
		return nil, errors.New("invalid bug report")
	}
	delete(input, "client")
	diagnostics, _ := ctx.Value(bugDiagnosticsKey{}).(*clientdiag.ClientDiagnostics)
	if diagnostics == nil {
		diagnostics = baseBugDiagnostics(ctx, target)
	}
	raw, err := json.Marshal(diagnostics)
	if err != nil {
		return nil, errors.New("bug report diagnostics unavailable")
	}
	input["client"] = raw
	raw, err = json.Marshal(input)
	if err != nil {
		return nil, errors.New("invalid bug report")
	}
	return raw, nil
}
