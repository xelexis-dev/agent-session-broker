package mcpserver

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
)

const bugReportDescription = "Report an x-broker bug to the administrator with reproduction steps. Never include tokens, keys, passwords, secrets or private file contents. See xbroker_docs bug-reports."

type bugReportOutput struct {
	ID         string    `json:"id,omitempty"`
	ReceivedAt time.Time `json:"receivedAt,omitzero"`
	Message    string    `json:"message,omitempty"`
	Code       string    `json:"code,omitempty"`
	RetryAt    time.Time `json:"retryAt,omitzero"`
}

func addBugReportTool(server *mcp.Server, backend Backend, version string) {
	mcp.AddTool(server, &mcp.Tool{Name: "report_bug", Description: bugReportDescription}, func(ctx context.Context, _ *mcp.CallToolRequest, in bugreport.Input) (*mcp.CallToolResult, bugReportOutput, error) {
		source, ok := backend.(interface {
			BugReportSession(context.Context) (broker.Session, error)
		})
		grant, granted := access.FromContext(ctx)
		if !ok || !granted {
			return nil, bugReportOutput{}, errors.New("reporter-unavailable")
		}
		session, err := source.BugReportSession(ctx)
		if err != nil {
			return nil, bugReportOutput{}, errors.New("reporter-unavailable")
		}
		store := bugreport.FromContext(ctx)
		if store == nil {
			return nil, bugReportOutput{}, errors.New("bug report store unavailable")
		}
		report, err := store.Create(in, bugreport.Reporter{ProjectGenerationID: grant.ProjectGenerationID, Project: grant.Project, SessionName: session.Name, RegistrationID: session.RegistrationID, Harness: session.Harness, OwnerID: grant.OwnerID}, version)
		var limited *bugreport.RateLimitError
		if errors.As(err, &limited) {
			return &mcp.CallToolResult{IsError: true}, bugReportOutput{Code: "rate-limited", RetryAt: limited.RetryAt}, nil
		}
		if err != nil {
			return nil, bugReportOutput{}, err
		}
		return nil, bugReportOutput{ID: report.ID, ReceivedAt: report.ReceivedAt, Message: "관리자에게 전달했습니다"}, nil
	})
}
