package channel

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestRenewContentDecoderOldAndNewServer(t *testing.T) {
	want := broker.RegisterResult{MessageAckVersion: 1, Session: broker.Session{RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Name: "synthetic", Generation: 7}}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"structured-only", "old-duplicate", "old-text-only"} {
		t.Run(mode, func(t *testing.T) {
			r := &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: want}
			if mode != "structured-only" {
				r.Content = []mcp.Content{&mcp.TextContent{Text: string(raw)}}
			}
			if mode == "old-text-only" {
				r.StructuredContent = nil
			}
			var got broker.RegisterResult
			if err := call(t.Context(), &oldScopeServer{result: r}, "renew_lease", broker.RenewRequest{}, &got); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatal("renew decode compatibility")
			}
		})
	}
}
