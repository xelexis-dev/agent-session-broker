package channel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func readDocs(t *testing.T, client *mcp.ClientSession, topic string) (*mcp.CallToolResult, docsOutput) {
	t.Helper()
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "xbroker_docs", Arguments: map[string]string{"topic": topic}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out docsOutput
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return result, out
}

func TestLocalDocsUpgradeMatchesREADME(t *testing.T) {
	up := &docsCountingUpstream{}
	client := catalogClient(t, newClientToolServer(up, nil, nil, "0.17.3"))
	result, out := readDocs(t, client, "upgrade")
	if result.IsError {
		t.Fatal("local upgrade docs unavailable")
	}
	readme, err := os.ReadFile("../../../README_AGENT.md")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Split(strings.Split(string(readme), "<!-- xbroker-docs:upgrade -->\n")[1], "<!-- /xbroker-docs:upgrade -->")[0]
	if out.Markdown != strings.TrimSpace(body)+"\n" || out.ClientVersion != "0.17.3" {
		t.Fatal("local docs differ from README or client version")
	}
	if up.calls.Load() != 0 {
		t.Fatal("docs reached upstream")
	}
}

func TestLocalDocsREADMEGeneratedEmbeddedAndCaps(t *testing.T) {
	readme, err := os.ReadFile("../../../README_AGENT.md")
	if err != nil {
		t.Fatal(err)
	}
	// Independent extraction checks today's README, not just a matching pair of stale assets.
	pairs := regexp.MustCompile(`(?s)<!-- xbroker-docs:([^\s>]+) -->(.*?)<!-- /xbroker-docs:([^\s>]+) -->`).FindAllSubmatch(readme, -1)
	if strings.Count(string(readme), "<!-- xbroker-docs:") != len(pairs) || strings.Count(string(readme), "<!-- /xbroker-docs:") != len(pairs) {
		t.Fatal("unmatched or nested README marker")
	}
	want := map[string]bool{"setup": true, "upgrade": true, "names": true, "delivery": true, "errors": true, "bug-reports": true, "kimi": true}
	seen := map[string]bool{}
	total := 0
	client := catalogClient(t, newClientToolServer(&docsCountingUpstream{}, nil, nil, "0.17.3"))
	for _, pair := range pairs {
		topic := string(pair[1])
		if !want[topic] || seen[topic] || topic != string(pair[3]) {
			t.Fatalf("invalid README markers for %q", topic)
		}
		seen[topic] = true
		body := strings.TrimSpace(string(pair[2])) + "\n"
		if strings.ContainsFunc(body, func(r rune) bool { return unicode.Is(unicode.Hangul, r) }) {
			t.Errorf("%s: client help must be English", topic)
		}
		generated, err := os.ReadFile("docs/" + topic + ".md")
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := clientDocs.ReadFile("docs/" + topic + ".md")
		if err != nil {
			t.Fatal(err)
		}
		result, out := readDocs(t, client, topic)
		if result.IsError || out.Markdown != body || string(generated) != body || string(embedded) != body {
			t.Errorf("%s: README != generated != embedded != response", topic)
		}
		if len(body) > 4096 {
			t.Errorf("%s exceeds 4 KiB", topic)
		}
		total += len(body)
	}
	files, err := clientDocs.ReadDir("docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(want) || len(files) != len(want) || total > 40960 {
		t.Fatal("topic set or total size mismatch")
	}
	if len(docsIndex) > 1024 {
		t.Fatal("index exceeds 1 KiB")
	}
}

func TestLocalDocsIndexAndUnknown(t *testing.T) {
	up := &docsCountingUpstream{}
	// An upstream cannot override the local tool.
	client := catalogClient(t, newClientToolServer(up, []*mcp.Tool{{Name: "xbroker_docs", InputSchema: map[string]any{"type": "object"}}}, nil, "0.17.3"))
	for _, topic := range []string{"", "missing", "../README", strings.Repeat("x", 8192)} {
		result, out := readDocs(t, client, topic)
		if out.ClientVersion != "0.17.3" || out.Markdown != docsIndex || len(out.Markdown) > 1024 {
			t.Fatal("missing bounded index/version")
		}
		if strings.ContainsFunc(out.Markdown, func(r rune) bool { return unicode.Is(unicode.Hangul, r) }) {
			t.Fatal("help index must be English")
		}
		if (topic != "") != result.IsError || (topic != "" && out.Code != "unknown-topic") {
			t.Fatal("unknown topic not classified")
		}
	}
	listed, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "xbroker_docs" && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint) {
			t.Error("docs not marked read-only")
		}
	}
	for _, tool := range snapshotTools() {
		if tool.Name == "xbroker_docs" {
			t.Error("local docs leaked into upstream snapshot")
		}
	}
	if up.calls.Load() != 0 {
		t.Fatal("docs forwarded upstream")
	}
}

type docsCountingUpstream struct {
	calls        atomic.Int32
	incompatible bool
}

func (u *docsCountingUpstream) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.calls.Add(1)
	return nil, errors.New("fixture offline")
}
func (u *docsCountingUpstream) compatibilityInfo() compatibilityState {
	if u.incompatible {
		return assessCompatibility("1.0.0", "0.11.9")
	}
	return compatibilityState{Compatible: true}
}

type docsCountingAuth struct{ calls atomic.Int32 }

func (a *docsCountingAuth) requestCode(context.Context, deviceCodeRequest) (deviceAuthorization, error) {
	a.calls.Add(1)
	return deviceAuthorization{}, errors.New("fixture authorization unavailable")
}
func (a *docsCountingAuth) redeem(context.Context, string) (sessionGrant, tokenOutcome, error) {
	a.calls.Add(1)
	return sessionGrant{}, tokenGranted, errors.New("fixture authorization unavailable")
}

func TestLocalDocsAvailableThroughClientGates(t *testing.T) {
	for _, mode := range []string{"reconnect-required", "incompatible", "offline", "unregistered", "kimi-identity-failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(mcpconfig.WithVersion(t.Context(), "1.0.0"))
			defer cancel()
			up := &docsCountingUpstream{incompatible: mode == "incompatible"}
			auth := &docsCountingAuth{}
			cwd := t.TempDir()
			var identityCalls atomic.Int32
			identify := func(_ context.Context, req *mcp.CallToolRequest) (broker.AttachRequest, error) {
				if req == nil {
					return broker.AttachRequest{}, errors.New("no startup identity")
				}
				identityCalls.Add(1)
				return broker.AttachRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: cwd}, nil
			}
			store, err := clientstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			opts := durableOptions{enabled: true, tokenMode: true, state: store, project: "docs", scope: "docs",
				device: &deviceAuthOptions{authorizer: auth, newUpstream: func(string) Upstream { return up }},
			}
			if mode == "reconnect-required" {
				opts.bindingCreated = func(b *ingressBinding) { b.receiveReason = "reconnect-required" }
			}
			if mode == "kimi-identity-failure" {
				opts.identityFailure = errKimiReconnect
			}
			server, shutdown := newIngressServerWithOptions(ctx, up, snapshotTools(), identify, nil, opts)
			defer func() { cancel(); shutdown() }()
			client := catalogClient(t, server)
			if mode == "reconnect-required" {
				// A failed explicit connection leaves a real binding for the recovery gate.
				_, _ = client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
				if auth.calls.Load() != 1 {
					t.Fatal("recovery fixture did not create a binding")
				}
			}
			beforeAuth, beforeUp, beforeIdentity := auth.calls.Load(), up.calls.Load(), identityCalls.Load()
			for _, topic := range []string{"setup", "upgrade", "", "unknown"} {
				result, out := readDocs(t, client, topic)
				if result.IsError != (topic == "unknown") || out.ClientVersion != "1.0.0" || out.Markdown == "" {
					t.Fatal("docs blocked by", mode)
				}
			}
			if auth.calls.Load() != beforeAuth || up.calls.Load() != beforeUp || identityCalls.Load() != beforeIdentity {
				t.Fatal("docs caused auth/upstream/identity activity")
			}
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": "me", "to": "other", "body": "hello"}})
			if err == nil && !result.IsError {
				t.Fatal("send_message bypassed", mode)
			}
			if up.calls.Load() != beforeUp {
				t.Fatal("blocked send reached upstream")
			}
		})
	}
}

func TestLocalDocsSetupIsBoundedAndOffline(t *testing.T) {
	up := &docsCountingUpstream{}
	client := catalogClient(t, newClientToolServer(up, nil, nil, "0.17.3"))
	result, out := readDocs(t, client, "setup")
	if result.IsError || out.Code != "" {
		t.Fatal("setup topic unavailable")
	}
	if len(out.Markdown) > 3584 {
		t.Fatal("setup exceeds 3.5 KiB")
	}
	for _, want := range []string{"npm install -g @xelexis/x-broker@1.1.0", `"command": "x-broker"`, `command = "x-broker"`, "npx", "100 MB", "Windows", "auth_status", "attach_session", "upgrade", "errors"} {
		if !strings.Contains(out.Markdown, want) {
			t.Errorf("setup missing %s", want)
		}
	}
	if !strings.Contains(docsIndex, "setup:") {
		t.Error("setup missing from index")
	}
	tools, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "xbroker_docs" && !strings.Contains(tool.Description, "setup") {
			t.Error("setup missing from description")
		}
	}
	if up.calls.Load() != 0 {
		t.Fatal("setup reached upstream")
	}
}
