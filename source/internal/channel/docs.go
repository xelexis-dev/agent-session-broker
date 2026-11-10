package channel

import (
	"context"
	"embed"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:generate python3 ../../../scripts/generate-client-docs.py
//go:embed docs/*.md
var clientDocs embed.FS

const docsInstructions = " 절차와 오류 코드는 xbroker_docs 호출."
const docsIndex = "# x-broker 도움말\n\n- setup: 설치와 MCP 설정\n- upgrade: 연결된 채 업그레이드\n- names: 이름 예약과 stop/retire\n- delivery: 메시지 전달과 한 번만 응답하기\n- errors: 오류 코드와 조치\n- bug-reports: 버그 보고와 처리\n- kimi: Kimi hook 설정\n\ntopic을 지정해 본문을 읽으세요.\n"

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
