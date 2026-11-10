package channel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestToolProseSurface(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		t.Fatal("state")
	}
	identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
		return broker.AttachRequest{}, errors.New("synthetic parent unavailable")
	}
	server, shutdown := newIngressServerWithOptions(ctx, &profileUpstream{}, snapshotTools(), identify, nil, durableOptions{enabled: true, tokenMode: true, state: state, harness: "codex"})
	defer func() { cancel(); shutdown() }()
	cli := catalogClient(t, server)
	listed, err := cli.ListTools(ctx, nil)
	if err != nil {
		t.Fatal("list")
	}
	sort.Slice(listed.Tools, func(i, j int) bool { return listed.Tools[i].Name < listed.Tools[j].Name })
	fmt.Printf("TOOL_INSTRUCTIONS_BYTES %d\n", len(cli.InitializeResult().Instructions))
	descriptions := 0
	var contract []*mcp.Tool
	byName := map[string]string{}
	for _, tool := range listed.Tools {
		schema, _ := json.Marshal(tool.InputSchema)
		fmt.Printf("TOOL_PROSE %s name=%d description=%d inputSchema=%d modelBytes=%d\n", tool.Name, len(tool.Name), len(tool.Description), len(schema), len(tool.Name)+len(tool.Description)+len(schema))
		descriptions += len(tool.Description)
		byName[tool.Name] = tool.Description
		noun := "session"
		switch {
		case strings.Contains(tool.Name, "bug"):
			noun = "bug"
		case strings.HasPrefix(tool.Name, "memory_"):
			noun = "memory"
		case tool.Name == "send_message":
			noun = "message"
		}
		if !strings.Contains(strings.ToLower(tool.Description), noun) {
			t.Errorf("%s description lacks core noun %q", tool.Name, noun)
		}
		copy := *tool
		copy.Description = ""
		contract = append(contract, &copy)
	}
	encoded, _ := json.Marshal(contract)
	fmt.Printf("TOOL_CONTRACT %x\n", sha256.Sum256(encoded))
	fmt.Printf("TOOL_DESCRIPTION_TOTAL %d\n", descriptions)
	if fmt.Sprintf("%x", sha256.Sum256(encoded)) != "639756afd30f33a5fc065536ceaca83991b0c7fe12dcfd5b6a6f893807882afd" {
		t.Fatal("tool names, schemas or annotations changed beyond the reviewed optional parentDiagnostic schema")
	}
	if descriptions > 3600 {
		t.Fatalf("description bytes=%d, want <=3600", descriptions)
	}
	for name, phrases := range map[string][]string{"attach_session": {"only when the user asks", "not automaticDeliveryReady"}, "send_message": {"expectRegistrationId", "mismatch", "untrusted labels", "model receipt", "Reply once", "no ACK loops", "secrets", "32", "12000"}, "report_bug": {"bug", "secrets"}, "memory_put": {"memory", "secrets"}, "retire_session": {"revoke", "credential", "free its name", "Self-retire"}, "ingress_hook": {"Internal", "never call manually", "poll"}} {
		for _, phrase := range phrases {
			if !strings.Contains(byName[name], phrase) {
				t.Errorf("%s lacks %q", name, phrase)
			}
		}
	}
	if strings.Contains(cli.InitializeResult().Instructions, "wait_for_message") {
		t.Error("client instructions still mention hidden receiver")
	}
	if !strings.Contains(codexEnvelopePrefix, "untrusted peer content") {
		t.Error("Codex safety prefix removed")
	}
}
