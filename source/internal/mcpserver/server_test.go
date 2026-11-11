package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
)

type stubBackend struct{}

type wakeResultBackend struct{ stubBackend }

type degradedResultBackend struct{ stubBackend }

func (wakeResultBackend) SendMessage(context.Context, broker.Message) (broker.SendResult, error) {
	return broker.SendResult{
		OK:               true,
		Accepted:         true,
		DeliverySequence: 41,
		MeteredWake:      true,
		SessionID:        "new67890",
		WakeStatus:       broker.WakeConfirmed,
	}, nil
}

func (degradedResultBackend) SendMessage(_ context.Context, message broker.Message) (broker.SendResult, error) {
	if message.To != "dev" {
		return broker.SendResult{}, broker.ErrSessionNotFound
	}
	return broker.SendResult{
		OK:               true,
		Accepted:         true,
		DeliverySequence: 42,
		MeteredWake:      true,
		SessionID:        "new67890",
		WakeStatus:       broker.WakeDegraded,
		Warning:          broker.WakeWarningRosterMismatch,
		WarningMessage:   "The accepted message could not confirm the resumed Claude session.",
	}, nil
}

func (stubBackend) ListSessions(context.Context) ([]broker.Session, error) {
	return []broker.Session{{Name: "dev", Harness: "codex", Status: broker.StatusIdle}}, nil
}

func TestSessionToolMetadataExplainsNameBasedCommunicationAddressing(t *testing.T) {
	ctx := context.Background()
	server := New(stubBackend{}, &stubMemory{})
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]*mcp.Tool, len(listed.Tools))
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}

	for _, name := range []string{"list_sessions", "get_session", "send_message", "wait_for_message", "stop_session"} {
		description := byName[name].Description
		if !strings.Contains(description, "Session.Name") {
			t.Errorf("%s description does not identify Session.Name as the communication address: %q", name, description)
		}
	}
	for _, name := range []string{"get_session", "send_message", "wait_for_message", "stop_session"} {
		encoded, err := json.Marshal(byName[name])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), "Session.Name communication address") {
			t.Errorf("%s schema does not explain name-based addressing: %s", name, encoded)
		}
	}
	sendDescription := byName["send_message"].Description
	for _, required := range []string{
		"calling agent interprets",
		"interpret 'do this development task in session mes-dev' as",
		`send_message(to: "mes-dev", body: "Do this development task.")`,
	} {
		if !strings.Contains(sendDescription, required) {
			t.Errorf("send_message description is missing %q: %q", required, sendDescription)
		}
	}
	sendSchema, err := json.Marshal(byName["send_message"].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sendSchema), `"batch"`) {
		t.Fatalf("send_message input schema exposes receive-only batch: %s", sendSchema)
	}
	if !strings.Contains(byName["wait_for_message"].Description, "batch.messages") || !strings.Contains(byName["wait_for_message"].Description, "FIFO") {
		t.Fatalf("wait_for_message description omits FIFO batch contract: %q", byName["wait_for_message"].Description)
	}
	for _, name := range []string{"list_sessions", "send_message"} {
		description := byName[name].Description
		for _, required := range []string{
			"stable Session.Name",
			"rotating Claude ID",
			"one metered Claude turn",
			"accepted",
			"confirmed",
			"degraded",
			"sessionId",
			"list_sessions",
		} {
			if !strings.Contains(description, required) {
				t.Errorf("%s description is missing lifecycle contract %q: %q", name, required, description)
			}
		}
		for _, forbidden := range []string{"delivery execution", "automatic retry", "restart durability"} {
			if strings.Contains(description, forbidden) {
				t.Errorf("%s description overpromises %q: %q", name, forbidden, description)
			}
		}
	}
}
func (stubBackend) GetSession(context.Context, string) (broker.Session, error) {
	return broker.Session{Name: "dev", Harness: "codex", Status: broker.StatusIdle}, nil
}
func (stubBackend) SendMessage(context.Context, broker.Message) (broker.SendResult, error) {
	return broker.SendResult{OK: true, Accepted: true, WakeStatus: broker.WakeNotNeeded}, nil
}
func (stubBackend) WaitForMessage(context.Context, string) (broker.Message, error) {
	return broker.Message{To: "plan", Body: "hello"}, nil
}
func (stubBackend) StopSession(context.Context, string) error { return nil }

type stubMemory struct {
	put    memory.PutInput
	putErr error
	get    string
	search memory.SearchInput
}

func (s *stubMemory) Put(_ context.Context, in memory.PutInput) (memory.Node, error) {
	s.put = in
	if s.putErr != nil {
		return memory.Node{}, s.putErr
	}
	return memory.Node{ID: "mem-put", Title: in.Title, Body: in.Body, CreatedBy: in.CreatedBy, CreatedAt: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC), Tags: in.Tags, Links: in.Links, Supersedes: in.Supersedes, Archived: in.Archived}, nil
}

func (s *stubMemory) Get(_ context.Context, id string) (memory.GetResult, error) {
	s.get = id
	return memory.GetResult{Node: memory.Node{ID: id, Title: "stored", CreatedBy: "dev", CreatedAt: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)}}, nil
}

func (s *stubMemory) Search(_ context.Context, in memory.SearchInput) (memory.SearchResult, error) {
	s.search = in
	return memory.SearchResult{Nodes: []memory.Summary{{ID: "mem-search", Title: "matching", CreatedBy: "dev", CreatedAt: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)}}, Warnings: []memory.Warning{}}, nil
}

func TestServerExposesExactlyEightToolsAndCallsBackends(t *testing.T) {
	ctx := context.Background()
	memories := &stubMemory{}
	server := New(stubBackend{}, memories)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"get_session", "list_sessions", "memory_get", "memory_put", "memory_search", "send_message", "stop_session", "wait_for_message"}
	if join(names) != join(want) {
		t.Fatalf("tools=%v want=%v", names, want)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %#v", res.Content)
	}
	put := memory.PutInput{Title: "decision", Body: "Use explicit links.", CreatedBy: "dev", Tags: []string{"design"}, Links: []string{"mem-parent"}, Supersedes: "mem-old", Archived: true}
	callMemoryTool(t, ctx, cs, "memory_put", put)
	if memories.put.Title != put.Title || memories.put.Body != put.Body || memories.put.CreatedBy != put.CreatedBy || join(memories.put.Tags) != join(put.Tags) || join(memories.put.Links) != join(put.Links) || memories.put.Supersedes != put.Supersedes || memories.put.Archived != put.Archived {
		t.Fatalf("memory_put input = %#v, want %#v", memories.put, put)
	}
	callMemoryTool(t, ctx, cs, "memory_put", map[string]any{"title": "default archive", "body": "body", "created_by": "dev"})
	if memories.put.Archived {
		t.Fatalf("memory_put omitted archived = %t, want false", memories.put.Archived)
	}
	callMemoryTool(t, ctx, cs, "memory_get", map[string]any{"id": "mem-put"})
	if memories.get != "mem-put" {
		t.Fatalf("memory_get id = %q, want %q", memories.get, "mem-put")
	}
	search := memory.SearchInput{Query: "explicit", Tags: []string{"design"}, LinkedTo: "mem-parent", CreatedBy: "dev", IncludeHistory: true, Limit: 42}
	callMemoryTool(t, ctx, cs, "memory_search", search)
	if memories.search.Query != search.Query || join(memories.search.Tags) != join(search.Tags) || memories.search.LinkedTo != search.LinkedTo || memories.search.CreatedBy != search.CreatedBy || memories.search.IncludeHistory != search.IncludeHistory || memories.search.Limit != search.Limit {
		t.Fatalf("memory_search input = %#v, want %#v", memories.search, search)
	}
}

func TestSendMessageToolReturnsAcceptedWakeMetadata(t *testing.T) {
	ctx := context.Background()
	server := New(wakeResultBackend{}, &stubMemory{})
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"from": "peer", "to": "dev", "body": "private body must not echo",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("send_message result = %#v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	content := string(encoded)
	for _, required := range []string{`"ok":true`, `"accepted":true`, `"deliverySequence":41`, `"meteredWake":true`, `"sessionId":"new67890"`, `"wakeStatus":"confirmed"`} {
		if !strings.Contains(content, required) {
			t.Errorf("send_message structured result missing %s: %s", required, content)
		}
	}
	for _, forbidden := range []string{"private body must not echo", "warningMessage", `C:\\private`} {
		if strings.Contains(content, forbidden) {
			t.Errorf("send_message structured result leaked %q: %s", forbidden, content)
		}
	}
}

func TestSendMessageAcceptedDegradedIsSanitizedNormalToolResult(t *testing.T) {
	ctx := context.Background()
	server := New(degradedResultBackend{}, &stubMemory{})
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"from": "foreign-project-label", "to": "dev", "body": `private body C:\secret full=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa`,
	}})
	if err != nil || result.IsError {
		t.Fatalf("degraded send_message = (%#v, %v)", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	content := string(encoded)
	for _, required := range []string{`"ok":true`, `"accepted":true`, `"deliverySequence":42`, `"meteredWake":true`, `"sessionId":"new67890"`, `"wakeStatus":"degraded"`, `"warning":"roster-mismatch"`, `"warningMessage":"The accepted message could not confirm the resumed Claude session."`} {
		if !strings.Contains(content, required) {
			t.Errorf("degraded result missing %s: %s", required, content)
		}
	}
	for _, forbidden := range []string{"foreign-project-label", "private body", `C:\secret`, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("degraded result exposed %q: %s", forbidden, content)
		}
	}

	foreign := callToolErrorText(t, ctx, clientSession, "foreign-id")
	absent := callToolErrorText(t, ctx, clientSession, "absent-id")
	if foreign != absent || !strings.Contains(foreign, broker.ErrSessionNotFound.Error()) {
		t.Fatalf("foreign/absent errors = %q / %q", foreign, absent)
	}
}

func callToolErrorText(t *testing.T, ctx context.Context, client *mcp.ClientSession, selector string) string {
	t.Helper()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": "caller", "to": selector, "body": "denied"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("send_message(%q) = %#v, want tool error", selector, result)
	}
	var content string
	for _, item := range result.Content {
		if text, ok := item.(*mcp.TextContent); ok {
			content += text.Text
		}
	}
	return content
}

func TestServerStillExposesExactlyEightTools(t *testing.T) {
	ctx := context.Background()
	server := New(stubBackend{}, &stubMemory{})
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 8 {
		t.Fatalf("tool count = %d", len(listed.Tools))
	}
}

func TestMemoryPutReportsBackendError(t *testing.T) {
	ctx := context.Background()
	server := New(stubBackend{}, &stubMemory{putErr: errors.New("write failed")})
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "memory_put", Arguments: map[string]any{"title": "decision", "body": "body", "created_by": "dev"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("memory_put backend error was not reported: %#v", res)
	}
}

func TestGetToolErrorKeepsSafeContentWhenSDKDropsOutput(t *testing.T) {
	service := broker.NewRemoteService()
	t.Cleanup(func() { service.Close() })
	client := catalogSession(t, New(getErrorBackend{service}, &stubMemory{}))
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_session", Arguments: map[string]any{"name": "dev"}})
	if err != nil {
		t.Fatalf("CallTool protocol error: %v", err)
	}
	if !result.IsError || result.StructuredContent != nil {
		t.Fatalf("SDK error projection: %+v", result)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "synthetic safe get error" {
		t.Fatalf("safe error text: %+v", result.Content)
	}
}

func TestServerRejectsNilMemoryBackend(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a nil memory backend")
		}
	}()
	New(stubBackend{}, nil)
}

type fakeProjectDirectory struct{ path string }

func (d fakeProjectDirectory) CanonicalPath() string { return d.path }
func (d fakeProjectDirectory) OperationPath() string { return d.path }
func (fakeProjectDirectory) Validate() error         { return nil }

func callMemoryTool(t *testing.T, ctx context.Context, client *mcp.ClientSession, name string, arguments any) {
	t.Helper()
	res, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s tool error: %#v", name, res.Content)
	}
}
func join(v []string) string {
	var s string
	for _, x := range v {
		s += "|" + x
	}
	return s
}

type getErrorBackend struct{ *broker.RemoteService }

type mcpFailingRegistry struct{}

func (mcpFailingRegistry) ProjectStorage(_ context.Context, project string) (groups.ProjectDirectory, error) {
	return fakeProjectDirectory{path: project}, nil
}

func (getErrorBackend) GetSession(context.Context, string) (broker.Session, error) {
	return broker.Session{ID: "synthetic-output-discarded"}, errors.New("synthetic safe get error")
}
