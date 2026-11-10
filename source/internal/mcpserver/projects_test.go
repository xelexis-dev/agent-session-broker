package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
)

func TestProjectHandlerRejectsInvalidProjectBeforeMCPHandling(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "missing", target: "/mcp"},
		{name: "empty", target: "/mcp?project="},
		{name: "repeated", target: "/mcp?project=mes&project=erp"},
		{name: "uppercase", target: "/mcp?project=Mes"},
		{name: "traversal", target: "/mcp?project=..%2Fmes"},
		{name: "overlong", target: "/mcp?project=" + strings.Repeat("a", 65)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			servers := &recordingProjectServers{server: New(stubBackend{}, &stubMemory{})}
			handler := ProjectHandler(servers)
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader("not MCP"))
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
			if projects := servers.projects(); len(projects) != 0 {
				t.Fatalf("MCP server factory projects = %v, want no call", projects)
			}
		})
	}
}

func TestProjectHandlerPassesCanonicalProjectToMCPFactory(t *testing.T) {
	servers := &recordingProjectServers{server: New(stubBackend{}, &stubMemory{})}
	handler := ProjectHandler(servers)
	request := newInitializeRequest("/mcp?project=mes.api-2")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	projects := servers.projects()
	if len(projects) != 1 || projects[0] != "mes.api-2" {
		t.Fatalf("MCP server factory projects = %v, want [mes.api-2]", projects)
	}
}

func TestProjectHandlerLetsSDKPreflightRejectWithoutInitializingProject(t *testing.T) {
	tests := []struct {
		name    string
		request func() *http.Request
		status  int
	}{
		{name: "GET", request: func() *http.Request { return httptest.NewRequest(http.MethodGet, "/mcp?project=mes", nil) }, status: http.StatusMethodNotAllowed},
		{name: "HEAD", request: func() *http.Request { return httptest.NewRequest(http.MethodHead, "/mcp?project=mes", nil) }, status: http.StatusMethodNotAllowed},
		{name: "OPTIONS", request: func() *http.Request { return httptest.NewRequest(http.MethodOptions, "/mcp?project=mes", nil) }, status: http.StatusMethodNotAllowed},
		{name: "origin", request: func() *http.Request {
			request := newInitializeRequest("/mcp?project=mes")
			request.Header.Set("Origin", "https://attacker.example")
			return request
		}, status: http.StatusForbidden},
		{name: "host", request: func() *http.Request {
			request := newInitializeRequest("/mcp?project=mes")
			request.Host = "attacker.example"
			local := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8787}
			return request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, local))
		}, status: http.StatusForbidden},
		{name: "accept", request: func() *http.Request {
			request := newInitializeRequest("/mcp?project=mes")
			request.Header.Set("Accept", "application/json")
			return request
		}, status: http.StatusBadRequest},
		{name: "content type", request: func() *http.Request {
			request := newInitializeRequest("/mcp?project=mes")
			request.Header.Set("Content-Type", "text/plain")
			return request
		}, status: http.StatusUnsupportedMediaType},
		{name: "protocol", request: func() *http.Request {
			request := newInitializeRequest("/mcp?project=mes")
			request.Header.Set("MCP-Protocol-Version", "2024-01-01")
			return request
		}, status: http.StatusBadRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			servers := &recordingProjectServers{server: New(stubBackend{}, &stubMemory{})}
			handler := ProjectHandler(servers)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, test.request())
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%q", response.Code, test.status, response.Body.String())
			}
			if projects := servers.projects(); len(projects) != 0 {
				t.Fatalf("SDK preflight initialized project server: %v", projects)
			}
		})
	}
}

func TestSDKPreflightFailuresCreateNoProjectDirectoriesStoresOrHandles(t *testing.T) {
	root := t.TempDir()
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	tracked := &trackingProjectRegistry{Registry: registry}
	var stores atomic.Int32
	factory := newProjectFactory(tracked, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		stores.Add(1)
		return &countedProjectStore{}, nil
	})
	t.Cleanup(func() { _ = factory.Close() })
	handler := ProjectHandler(factory)

	requests := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/mcp?project=mes", nil),
		func() *http.Request {
			request := newInitializeRequest("/mcp?project=mes")
			request.Header.Set("Origin", "https://attacker.example")
			return request
		}(),
		func() *http.Request {
			request := newInitializeRequest("/mcp?project=mes")
			request.Header.Set("Accept", "application/json")
			return request
		}(),
		func() *http.Request {
			request := newInitializeRequest("/mcp?project=mes")
			request.Header.Set("Content-Type", "text/plain")
			return request
		}(),
		func() *http.Request {
			request := newInitializeRequest("/mcp?project=mes")
			request.Header.Set("MCP-Protocol-Version", "2024-01-01")
			return request
		}(),
	}
	for _, request := range requests {
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if got := tracked.storageCalls.Load(); got != 0 {
		t.Fatalf("ProjectStorage calls = %d, want zero pinned project handles", got)
	}
	if got := stores.Load(); got != 0 {
		t.Fatalf("Store constructions = %d, want zero", got)
	}
	projectDirectory := filepath.Join(root, "projects", "p-6d6573")
	if _, err := os.Stat(projectDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SDK preflight created project directory: %v", err)
	}
}

func TestProjectHandlerLeavesHealthUnscoped(t *testing.T) {
	servers := &recordingProjectServers{server: New(stubBackend{}, &stubMemory{})}
	handler := ProjectHandler(servers)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
		t.Fatalf("health response = (%d, %q), want (200, %q)", response.Code, response.Body.String(), "ok\n")
	}
	if projects := servers.projects(); len(projects) != 0 {
		t.Fatalf("health initialized project servers: %v", projects)
	}
}

func TestProjectFactoryCachesOneServerAndStorePerProjectConcurrently(t *testing.T) {
	memoryRoot := t.TempDir()
	registry, err := groups.NewRegistry(filepath.Join(memoryRoot, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	var calls atomic.Int32
	var rootsMu sync.Mutex
	var roots []string
	store := &countedProjectStore{}
	factory := newProjectFactory(registry, func(project groups.ProjectDirectory) (projectMemoryStore, error) {
		calls.Add(1)
		rootsMu.Lock()
		roots = append(roots, project.CanonicalPath())
		rootsMu.Unlock()
		return store, nil
	})
	t.Cleanup(func() {
		if err := factory.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	const callers = 32
	servers := make(chan *mcp.Server, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			ready.Done()
			<-start
			server, err := factory.Server(context.Background(), "mes")
			servers <- server
			errs <- err
		}()
	}
	ready.Wait()
	close(start)

	var first *mcp.Server
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("Server() error = %v", err)
		}
		server := <-servers
		if first == nil {
			first = server
		} else if server != first {
			t.Fatal("concurrent calls returned different project servers")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("memory Store constructions = %d, want 1", got)
	}
	rootsMu.Lock()
	defer rootsMu.Unlock()
	canonicalMemoryRoot, err := filepath.EvalSymlinks(memoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(canonicalMemoryRoot, "projects", "p-6d6573")
	if len(roots) != 1 || roots[0] != wantRoot {
		t.Fatalf("memory Store roots = %v, want [%s]", roots, wantRoot)
	}
}

func TestProjectFactoryBlockedInitializationDoesNotBlockAnotherProject(t *testing.T) {
	registry := &blockingProjectRegistry{
		mcpFailingRegistry: mcpFailingRegistry{},
		blockedProject:     "mes",
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	factory := newProjectFactory(registry, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		return &countedProjectStore{}, nil
	})
	t.Cleanup(func() { _ = factory.Close() })

	blockedResult := make(chan error, 1)
	go func() {
		_, err := factory.Server(context.Background(), "mes")
		blockedResult <- err
	}()
	<-registry.entered

	type serverResult struct {
		server *mcp.Server
		err    error
	}
	otherResult := make(chan serverResult, 1)
	go func() {
		server, err := factory.Server(context.Background(), "erp")
		otherResult <- serverResult{server: server, err: err}
	}()
	var other serverResult
	select {
	case other = <-otherResult:
	case <-time.After(time.Second):
		close(registry.release)
		<-blockedResult
		<-otherResult
		t.Fatal("Server(erp) remained blocked behind mes initialization")
	}
	if other.err != nil || other.server == nil {
		close(registry.release)
		<-blockedResult
		t.Fatalf("Server(erp) while mes initialization is blocked = (%p, %v), want independent server", other.server, other.err)
	}

	close(registry.release)
	if err := <-blockedResult; err != nil {
		t.Fatalf("Server(mes) after initialization release error = %v", err)
	}
}

func TestProjectFactoryCloseDuringInitializationClosesStoreOnce(t *testing.T) {
	storeEntered := make(chan struct{})
	releaseStore := make(chan struct{})
	store := &countedProjectStore{}
	factory := newProjectFactory(mcpFailingRegistry{}, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		close(storeEntered)
		<-releaseStore
		return store, nil
	})

	serverResult := make(chan error, 1)
	go func() {
		_, err := factory.Server(context.Background(), "mes")
		serverResult <- err
	}()
	<-storeEntered
	closeCalled := make(chan struct{})
	closeResult := make(chan error, 1)
	go func() {
		close(closeCalled)
		closeResult <- factory.Close()
	}()
	<-closeCalled

	close(releaseStore)
	if err := <-serverResult; err != nil && !errors.Is(err, ErrProjectFactoryClosed) {
		t.Fatalf("initializing Server result = %v, want success or ErrProjectFactoryClosed at the race boundary", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := store.closeCalls.Load(); got != 1 {
		t.Fatalf("Store Close calls = %d, want 1", got)
	}
	if err := factory.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestProjectFactoryBackendFailureClosesStoreAndDoesNotCache(t *testing.T) {
	stores := []*countedProjectStore{{}, {}}
	calls := 0
	factory := newProjectFactory(mcpFailingRegistry{}, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		store := stores[calls]
		calls++
		return store, nil
	})
	t.Cleanup(func() { factory.Close() })
	failure := errors.New("synthetic backend construction failure")
	factory.newRemote = func(groups.ProjectDirectory) (Backend, error) { return nil, failure }
	if server, err := factory.Server(t.Context(), "mes"); server != nil || !errors.Is(err, failure) {
		t.Fatalf("failed construction: %p %v", server, err)
	}
	if stores[0].closeCalls.Load() != 1 || len(factory.projects) != 0 {
		t.Fatal("failed backend retained its store or cache")
	}
	factory.newRemote = func(groups.ProjectDirectory) (Backend, error) { return broker.NewRemoteService(), nil }
	if server, err := factory.Server(t.Context(), "mes"); server == nil || err != nil {
		t.Fatalf("retry: %p %v", server, err)
	}
	if calls != 2 || stores[1].closeCalls.Load() != 0 {
		t.Fatal("successful retry did not retain its store")
	}
}

func TestProjectFactoryClosesPartialConstructionAndDoesNotCacheFailure(t *testing.T) {
	buildErr := errors.New("store construction failed")
	partial := &countedProjectStore{}
	healthy := []*countedProjectStore{{}, {}}
	var calls int
	factory := newProjectFactory(mcpFailingRegistry{}, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		calls++
		if calls == 1 {
			return partial, buildErr
		}
		return healthy[calls-2], nil
	})

	if server, err := factory.Server(context.Background(), "mes"); server != nil || !errors.Is(err, buildErr) {
		t.Fatalf("first Server() = (%p, %v), want (nil, build error)", server, err)
	}
	if got := partial.closeCalls.Load(); got != 1 {
		t.Fatalf("partial Store Close calls = %d, want 1", got)
	}
	if server, err := factory.Server(context.Background(), "mes"); err != nil || server == nil {
		t.Fatalf("retry Server() = (%p, %v), want non-nil server", server, err)
	}
	if server, err := factory.Server(context.Background(), "erp"); err != nil || server == nil {
		t.Fatalf("second project Server() = (%p, %v), want non-nil server", server, err)
	}
	if calls != 3 {
		t.Fatalf("Store constructions = %d, want failed entry plus two projects", calls)
	}
	if err := factory.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := factory.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	for index, store := range healthy {
		if got := store.closeCalls.Load(); got != 1 {
			t.Fatalf("cached Store %d Close calls = %d, want 1", index, got)
		}
	}
	if server, err := factory.Server(context.Background(), "mes"); server != nil || !errors.Is(err, ErrProjectFactoryClosed) {
		t.Fatalf("Server() after Close = (%p, %v), want ErrProjectFactoryClosed", server, err)
	}
}

func TestProjectFactoryRejectsTypedNilStore(t *testing.T) {
	buildErr := errors.New("store construction failed")
	tests := []struct {
		name     string
		buildErr error
	}{
		{name: "construction error", buildErr: buildErr},
		{name: "nil success"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var store *countedProjectStore
			factory := newProjectFactory(mcpFailingRegistry{}, func(groups.ProjectDirectory) (projectMemoryStore, error) {
				return store, test.buildErr
			})

			server, err := factory.Server(context.Background(), "mes")
			if server != nil || err == nil {
				t.Fatalf("Server() = (%p, %v), want (nil, error)", server, err)
			}
			if test.buildErr != nil && !errors.Is(err, test.buildErr) {
				t.Fatalf("Server() error = %v, want construction error", err)
			}
		})
	}
}

func TestProjectHandlerSanitizesTypedNilStoreConstructionFailure(t *testing.T) {
	var store *countedProjectStore
	factory := newProjectFactory(mcpFailingRegistry{}, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		return store, errors.New("private store construction failure")
	})
	response := httptest.NewRecorder()

	ProjectHandler(factory).ServeHTTP(response, newInitializeRequest("/mcp?project=mes"))

	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, want MCP JSON-RPC response; body=%q", response.Code, response.Body.String())
	}
	var payload struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	if payload.Error == nil || payload.Error.Message != "project service unavailable" {
		t.Fatalf("response = %s, want sanitized project service unavailable error", response.Body.String())
	}
}

func TestValidInitializeCreatesProjectOnce(t *testing.T) {
	root := t.TempDir()
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	var constructions atomic.Int32
	factory := newProjectFactory(registry, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		constructions.Add(1)
		return &countedProjectStore{}, nil
	})
	t.Cleanup(func() { _ = factory.Close() })
	handler := ProjectHandler(factory)

	for range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, newInitializeRequest("/mcp?project=mes"))
		if response.Code != http.StatusOK {
			t.Fatalf("initialize status = %d, want 200; body=%q", response.Code, response.Body.String())
		}
	}
	if got := constructions.Load(); got != 1 {
		t.Fatalf("Store constructions = %d, want 1", got)
	}
}

func TestProjectFactoryCapacityReturnsSanitizedMCPErrorWithoutEvictionOrDirectory(t *testing.T) {
	root := t.TempDir()
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	var constructions atomic.Int32
	factory := newProjectFactory(registry, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		constructions.Add(1)
		return &countedProjectStore{}, nil
	})
	t.Cleanup(func() { _ = factory.Close() })

	var first *mcp.Server
	for index := range MaxCachedProjects {
		project := fmt.Sprintf("p%03d", index)
		server, err := factory.Server(context.Background(), project)
		if err != nil {
			t.Fatalf("Server(%q) error = %v", project, err)
		}
		if index == 0 {
			first = server
		}
	}
	overflow := "overflow"
	overflowDirectory := filepath.Join(root, "projects", "p-"+fmt.Sprintf("%x", overflow))
	response := httptest.NewRecorder()
	ProjectHandler(factory).ServeHTTP(response, newInitializeRequest("/mcp?project="+overflow))
	if response.Code != http.StatusOK {
		t.Fatalf("capacity response status = %d, want MCP JSON-RPC response; body=%q", response.Code, response.Body.String())
	}
	var payload struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode capacity response %q: %v", response.Body.String(), err)
	}
	if payload.Error == nil || payload.Error.Message != "project capacity reached" {
		t.Fatalf("capacity response = %s, want sanitized project capacity error", response.Body.String())
	}
	for _, forbidden := range []string{overflow, root} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("capacity response leaked %q: %s", forbidden, response.Body.String())
		}
	}
	if _, err := os.Stat(overflowDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overflow project directory exists or cannot be checked: %v", err)
	}
	if got := constructions.Load(); got != MaxCachedProjects {
		t.Fatalf("Store constructions = %d, want capped %d", got, MaxCachedProjects)
	}
	server, err := factory.Server(context.Background(), "p000")
	if err != nil || server != first {
		t.Fatalf("first cached server after capacity = (%p, %v), want unchanged %p", server, err, first)
	}
}

func TestProjectFactoryCapacityBoundaryIsAtomic(t *testing.T) {
	root := t.TempDir()
	registry, err := groups.NewRegistry(filepath.Join(root, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	var constructions atomic.Int32
	factory := newProjectFactory(registry, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		constructions.Add(1)
		return &countedProjectStore{}, nil
	})
	t.Cleanup(func() { _ = factory.Close() })
	for index := range MaxCachedProjects - 1 {
		if _, err := factory.Server(context.Background(), fmt.Sprintf("p%03d", index)); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	errorsByProject := make(chan error, 2)
	for _, project := range []string{"boundary-a", "boundary-b"} {
		go func() {
			<-start
			_, err := factory.Server(context.Background(), project)
			errorsByProject <- err
		}()
	}
	close(start)
	var successes, capacityErrors int
	for range 2 {
		err := <-errorsByProject
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrProjectCapacity):
			capacityErrors++
		default:
			t.Fatalf("boundary Server() error = %v", err)
		}
	}
	if successes != 1 || capacityErrors != 1 {
		t.Fatalf("boundary outcomes = %d success, %d capacity; want 1 and 1", successes, capacityErrors)
	}
	if got := constructions.Load(); got != MaxCachedProjects {
		t.Fatalf("Store constructions = %d, want %d", got, MaxCachedProjects)
	}
}

func TestProjectFactoryFailedConstructionCannotBypassAdmissionCapacity(t *testing.T) {
	registry := &countingProjectRegistry{mcpFailingRegistry: mcpFailingRegistry{}}
	buildErr := errors.New("store construction failed")
	var constructions atomic.Int32
	factory := newProjectFactory(registry, func(groups.ProjectDirectory) (projectMemoryStore, error) {
		constructions.Add(1)
		return nil, buildErr
	})
	t.Cleanup(func() { _ = factory.Close() })

	for index := range MaxCachedProjects + 16 {
		project := fmt.Sprintf("f%03d", index)
		_, err := factory.Server(context.Background(), project)
		if index < MaxCachedProjects {
			if !errors.Is(err, buildErr) {
				t.Fatalf("Server(%q) error = %v, want construction failure", project, err)
			}
		} else if !errors.Is(err, ErrProjectCapacity) {
			t.Fatalf("Server(%q) error = %v, want ErrProjectCapacity", project, err)
		}
	}
	if got := registry.storageCalls.Load(); got != MaxCachedProjects {
		t.Fatalf("ProjectStorage calls = %d, want bounded %d", got, MaxCachedProjects)
	}
	if got := constructions.Load(); got != MaxCachedProjects {
		t.Fatalf("Store constructions = %d, want bounded %d", got, MaxCachedProjects)
	}
	if _, err := factory.Server(context.Background(), "f000"); !errors.Is(err, buildErr) {
		t.Fatalf("retry admitted failed project error = %v, want construction failure", err)
	}
	if got := registry.storageCalls.Load(); got != MaxCachedProjects+1 {
		t.Fatalf("retry ProjectStorage calls = %d, want %d", got, MaxCachedProjects+1)
	}
}

func TestProjectFactoryKeepsRemoteStateAcrossServerRequests(t *testing.T) {
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := NewRemoteProjectFactory(registry)
	defer factory.Close()
	service := attachProjectFixture(t, factory, "mes", "11111111-1111-4111-8111-111111111111", "worker")
	server := httptest.NewServer(ProjectHandler(factory))
	defer server.Close()
	first := connectProjectHTTP(t, server.URL+"/mcp?project=mes")
	sent := callStructuredTool[broker.SendResult](t, first, "send_message", map[string]any{"from": "worker", "to": "worker", "body": "survives HTTP reconnect"})
	if !sent.Accepted {
		t.Fatal("message not queued")
	}
	first.Close()
	second := connectProjectHTTP(t, server.URL+"/mcp?project=mes")
	sessions := callStructuredTool[[]broker.Session](t, second, "list_sessions", map[string]any{})
	if len(sessions) != 1 || sessions[0].PendingMessages != 1 {
		t.Fatal("cached registration or queue lost")
	}
	same, err := factory.RemoteBackend(t.Context(), "mes")
	if err != nil || same != service {
		t.Fatal("factory changed backend across requests")
	}
	message, err := service.WaitForIngress(t.Context(), "worker", "lease-mes")
	if err != nil || message.Body != "survives HTTP reconnect" {
		t.Fatalf("queue: %v", err)
	}
	foreign := connectProjectHTTP(t, server.URL+"/mcp?project=erp")
	refused, err := foreign.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"from": "worker", "to": "worker", "body": "foreign"}})
	if err != nil || !refused.IsError {
		t.Fatal("foreign project used cached registration")
	}
}

func TestProjectFactoryThenRegistryCloseReleaseAllPinnedProjectHandles(t *testing.T) {
	root := t.TempDir()
	projectsRoot := filepath.Join(root, "projects")
	registry, err := groups.NewRegistry(projectsRoot)
	if err != nil {
		t.Fatal(err)
	}
	factory := NewRemoteProjectFactory(registry)
	if _, err := factory.Server(context.Background(), "mes"); err != nil {
		t.Fatal(err)
	}
	if err := factory.Close(); err != nil {
		t.Fatalf("factory Close() error = %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("registry Close() error = %v", err)
	}
	if err := os.Rename(projectsRoot, filepath.Join(root, "moved-projects")); err != nil {
		t.Fatalf("rename projects root after graceful cleanup: %v", err)
	}
}

func TestProjectFactoryServersCaptureIndependentProjectsAndIngressTools(t *testing.T) {
	registry, err := groups.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	factory := NewRemoteProjectFactory(registry)
	defer factory.Close()
	ids := map[string]string{"mes": "11111111-1111-4111-8111-111111111111", "erp": "22222222-2222-4222-8222-222222222222"}
	for project, id := range ids {
		attachProjectFixture(t, factory, project, id, "worker")
	}
	server := httptest.NewServer(ProjectHandler(factory))
	defer server.Close()
	wantTools := []string{"attach_session", "get_bug_report", "get_session", "list_bug_reports", "list_sessions", "memory_get", "memory_put", "memory_search", "probe_response", "register_session", "rename_session", "renew_lease", "report_bug", "report_ingress_failure", "resume_session", "retire_session", "send_message", "set_bug_report_status", "stop_session", "suspend_session", "unsuspend_session", "wait_for_message"}
	for project, id := range ids {
		client := connectProjectHTTP(t, server.URL+"/mcp?project="+project)
		listed, err := client.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		if join(names) != join(wantTools) {
			t.Fatalf("%s tools=%v want=%v", project, names, wantTools)
		}
		sessions := callStructuredTool[[]broker.Session](t, client, "list_sessions", map[string]any{})
		if len(sessions) != 1 || sessions[0].ID != id || sessions[0].Name != "worker" {
			t.Fatalf("project leaked registration: %+v", sessions)
		}
	}
}

func TestProjectFactoryMemoryStoresIsolateNodesLinksAndLegacyRoot(t *testing.T) {
	memoryRoot := t.TempDir()
	legacy, err := memory.NewStore(memoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Put(context.Background(), memory.PutInput{Title: "legacy", Body: "not project owned", CreatedBy: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	registry, err := groups.NewRegistry(filepath.Join(memoryRoot, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	factory := NewRemoteProjectFactory(registry)
	t.Cleanup(func() { _ = factory.Close() })
	httpServer := httptest.NewServer(ProjectHandler(factory))
	t.Cleanup(httpServer.Close)

	mes := connectProjectHTTP(t, httpServer.URL+"/mcp?project=mes")
	erp := connectProjectHTTP(t, httpServer.URL+"/mcp?project=erp")
	created := callStructuredTool[memory.Node](t, mes, "memory_put", map[string]any{
		"title": "MES decision", "body": "keep this local", "created_by": "mes-dev",
	})
	mesSearch := callStructuredTool[memory.SearchResult](t, mes, "memory_search", map[string]any{"query": "local"})
	if len(mesSearch.Nodes) != 1 || mesSearch.Nodes[0].ID != created.ID {
		t.Fatalf("MES search = %#v, want created node %q", mesSearch, created.ID)
	}
	erpSearch := callStructuredTool[memory.SearchResult](t, erp, "memory_search", map[string]any{})
	if len(erpSearch.Nodes) != 0 {
		t.Fatalf("ERP search exposed MES or legacy nodes: %#v", erpSearch.Nodes)
	}
	getResult, err := erp.CallTool(context.Background(), &mcp.CallToolParams{Name: "memory_get", Arguments: map[string]any{"id": created.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if !getResult.IsError {
		t.Fatalf("ERP memory_get exposed MES node %q", created.ID)
	}
	linkResult, err := erp.CallTool(context.Background(), &mcp.CallToolParams{Name: "memory_put", Arguments: map[string]any{
		"title": "cross project", "body": "must fail", "created_by": "erp-dev", "links": []string{created.ID},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !linkResult.IsError {
		t.Fatalf("ERP memory_put accepted MES link %q", created.ID)
	}
}

type recordingProjectServers struct {
	mu      sync.Mutex
	server  *mcp.Server
	project []string
}

type trackingProjectRegistry struct {
	*groups.Registry
	storageCalls atomic.Int32
}

type countingProjectRegistry struct {
	mcpFailingRegistry
	storageCalls atomic.Int32
}

type blockingProjectRegistry struct {
	mcpFailingRegistry
	blockedProject string
	entered        chan struct{}
	release        chan struct{}
	once           sync.Once
}

func (r *blockingProjectRegistry) ProjectStorage(ctx context.Context, project string) (groups.ProjectDirectory, error) {
	if project != r.blockedProject {
		return r.mcpFailingRegistry.ProjectStorage(ctx, project)
	}
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		return r.mcpFailingRegistry.ProjectStorage(ctx, project)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *countingProjectRegistry) ProjectStorage(ctx context.Context, project string) (groups.ProjectDirectory, error) {
	r.storageCalls.Add(1)
	return r.mcpFailingRegistry.ProjectStorage(ctx, project)
}

func (r *trackingProjectRegistry) ProjectStorage(ctx context.Context, project string) (groups.ProjectDirectory, error) {
	r.storageCalls.Add(1)
	return r.Registry.ProjectStorage(ctx, project)
}

type countedProjectStore struct {
	closeCalls atomic.Int32
}

func (s *countedProjectStore) Close() error {
	s.closeCalls.Add(1)
	return nil
}

func (*countedProjectStore) Put(context.Context, memory.PutInput) (memory.Node, error) {
	return memory.Node{}, nil
}

func (*countedProjectStore) Get(context.Context, string) (memory.GetResult, error) {
	return memory.GetResult{}, nil
}

func (*countedProjectStore) Search(context.Context, memory.SearchInput) (memory.SearchResult, error) {
	return memory.SearchResult{}, nil
}

func connectProjectHTTP(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "project-http-test", Version: "0.17.3"}, nil)
	clientSession, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func callStructuredTool[T any](t *testing.T, client *mcp.ClientSession, name string, arguments any) T {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s tool error: %#v", name, result.Content)
	}
	content, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if result.StructuredContent == nil {
		if len(result.Content) != 1 {
			t.Fatalf("%s missing JSON text result", name)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("%s result is not text", name)
		}
		content = []byte(text.Text)
	}
	var output T
	if err := json.Unmarshal(content, &output); err != nil {
		t.Fatalf("decode %s structured content %s: %v", name, content, err)
	}
	return output
}

func (f *recordingProjectServers) Server(_ context.Context, project string) (*mcp.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.project = append(f.project, project)
	return f.server, nil
}

func newInitializeRequest(target string) *http.Request {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	return request
}

func (f *recordingProjectServers) projects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.project...)
}

func attachProjectFixture(t *testing.T, f *ProjectFactory, project, id, name string) *broker.RemoteService {
	t.Helper()
	backend, err := f.RemoteBackend(t.Context(), project)
	if err != nil {
		t.Fatal(err)
	}
	service := backend.(*broker.RemoteService)
	if _, err = service.AttachSession(t.Context(), broker.AttachRequest{Name: name, Harness: "codex", SessionID: id, PID: 123, CWD: "/synthetic", ChannelID: "lease-" + project}); err != nil {
		t.Fatal(err)
	}
	return service
}
