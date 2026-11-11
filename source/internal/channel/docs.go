package channel

import (
	"context"
	"embed"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:generate python3 ../../../scripts/generate-client-docs.py
//go:embed docs/*.md
var clientDocs embed.FS

const docsInstructions = " Call xbroker_docs for procedures and error codes."
const docsIndex = "# x-broker help\n\n- setup: Installation and MCP configuration\n- upgrade: Upgrading while connected\n- names: Name reservations and stop/retire\n- delivery: Message delivery and replying once\n- errors: Error codes and recovery\n- bug-reports: Reporting and handling bugs\n- kimi: Kimi hook configuration\n\nSpecify a topic to read it.\n"

type docsInput struct {
	Topic string `json:"topic,omitempty" jsonschema:"Optional help topic; omit for the topic index"`
}
type docsOutput struct {
	ClientVersion string `json:"clientVersion"`
	Code          string `json:"code,omitempty"`
	Markdown      string `json:"markdown"`
}

func addDocsTool(server *mcp.Server, version string) {
	mcp.AddTool(server, &mcp.Tool{Name: "xbroker_docs", Description: "Read local session/message/bug/memory procedures: setup, upgrade, names, delivery, errors, bug-reports, kimi. No registration or server call.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(_ context.Context, _ *mcp.CallToolRequest, in docsInput) (*mcp.CallToolResult, docsOutput, error) {
		out := docsOutput{ClientVersion: version, Markdown: docsIndex}
		if in.Topic == "" {
			return nil, out, nil
		}
		// Only bundled topic names are accepted; never interpret a caller's path.
		switch in.Topic {
		case "setup", "upgrade", "names", "delivery", "errors", "bug-reports", "kimi":
			body, err := clientDocs.ReadFile("docs/" + in.Topic + ".md")
			if err != nil {
				return nil, out, err
			}
			out.Markdown = string(body)
			return nil, out, nil
		default:
			out.Code = "unknown-topic"
			return &mcp.CallToolResult{IsError: true}, out, nil
		}
	})
}
