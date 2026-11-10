package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpconfig"
	"github.com/xelexis-dev/agent-session-broker/source/internal/memory"
)

var projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var (
	ErrInvalidProject       = errors.New("invalid project")
	ErrProjectFactoryClosed = errors.New("project service factory is closed")
	ErrProjectCapacity      = errors.New("project capacity reached")
)

// MaxCachedProjects bounds process-lifetime project services. Entries are
// never evicted because remote registration and queue state is load-bearing.
const MaxCachedProjects = 128

type ProjectServers interface {
	Server(context.Context, string) (*mcp.Server, error)
}

type ProjectRegistry interface {
	ProjectStorage(context.Context, string) (groups.ProjectDirectory, error)
}

type projectContextKey struct{}

type projectMemoryStore interface {
	MemoryBackend
	Close() error
}

type projectEntry struct {
	server *mcp.Server
	store  projectMemoryStore
}

type projectInitialization struct {
	ready  chan struct{}
	server *mcp.Server
	err    error
}

type ProjectFactory struct {
	version   string
	registry  ProjectRegistry
	newStore  func(groups.ProjectDirectory) (projectMemoryStore, error)
	newRemote func(groups.ProjectDirectory) (Backend, error)

	mu           sync.Mutex
	closed       bool
	closeReady   chan struct{}
	closeErr     error
	admitted     map[string]struct{}
	initializing map[string]*projectInitialization
	projects     map[string]projectEntry
}

// NewRemoteProjectFactory keeps shared memory on A and agent execution on B/C.
// No local harness driver is constructed or called for remote registrations.
func NewRemoteProjectFactory(registry ProjectRegistry, version ...string) *ProjectFactory {
	f := newProjectFactory(registry, func(project groups.ProjectDirectory) (projectMemoryStore, error) {
		return memory.NewStoreInProject(project)
	})
	f.version = mcpconfig.Version(version...)
	return f
}

func newProjectFactory(registry ProjectRegistry, newStore func(groups.ProjectDirectory) (projectMemoryStore, error)) *ProjectFactory {
	return &ProjectFactory{
		registry:     registry,
		newStore:     newStore,
		closeReady:   make(chan struct{}),
		admitted:     make(map[string]struct{}),
		initializing: make(map[string]*projectInitialization),
		projects:     make(map[string]projectEntry),
	}
}

func (f *ProjectFactory) Server(ctx context.Context, project string) (*mcp.Server, error) {
	if !projectPattern.MatchString(project) {
		return nil, fmt.Errorf("%w", ErrInvalidProject)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, fmt.Errorf("%w", ErrProjectFactoryClosed)
	}
	if entry, ok := f.projects[project]; ok {
		f.mu.Unlock()
		return entry.server, nil
	}
	if initialization, ok := f.initializing[project]; ok {
		f.mu.Unlock()
		return waitForProjectInitialization(ctx, initialization)
	}
	if _, admitted := f.admitted[project]; !admitted {
		if len(f.admitted) >= MaxCachedProjects {
			f.mu.Unlock()
			return nil, fmt.Errorf("%w", ErrProjectCapacity)
		}
		// Keep failed names admitted as well: ProjectStorage may already have
		// pinned a directory, so forgetting failures would make handle growth
		// unbounded. The same name may still retry without consuming a slot.
		f.admitted[project] = struct{}{}
	}
	initialization := &projectInitialization{ready: make(chan struct{})}
	f.initializing[project] = initialization
	f.mu.Unlock()

	projectDirectory, err := f.registry.ProjectStorage(ctx, project)
	if err != nil {
		return f.finishInitialization(project, initialization, nil, nil, err)
	}
	if projectDirectory == nil {
		return f.finishInitialization(project, initialization, nil, nil, errors.New("project directory is unavailable"))
	}
	if err := projectDirectory.Validate(); err != nil {
		return f.finishInitialization(project, initialization, nil, nil, err)
	}
	store, err := f.newStore(projectDirectory)
	if err != nil {
		if !isNilProjectMemoryStore(store) {
			if closeErr := store.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close partial project memory: %w", closeErr))
			}
		}
		return f.finishInitialization(project, initialization, nil, nil, err)
	}
	if isNilProjectMemoryStore(store) {
		return f.finishInitialization(project, initialization, nil, nil, errors.New("project memory store is unavailable"))
	}
	var remote Backend = broker.NewRemoteService()
	if f.newRemote != nil {
		remote, err = f.newRemote(projectDirectory)
		if err != nil || remote == nil {
			if err == nil {
				err = errors.New("remote backend unavailable")
			}
			if closer, ok := remote.(interface{ Close() error }); ok {
				err = errors.Join(err, closer.Close())
			}
			err = errors.Join(err, store.Close())
			return f.finishInitialization(project, initialization, nil, nil, err)
		}
	}
	store = &remoteProjectStore{projectMemoryStore: store, backend: remote}
	server := New(remote, store, f.version)
	return f.finishInitialization(project, initialization, server, store, nil)
}

func waitForProjectInitialization(ctx context.Context, initialization *projectInitialization) (*mcp.Server, error) {
	select {
	case <-initialization.ready:
		return initialization.server, initialization.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *ProjectFactory) finishInitialization(project string, initialization *projectInitialization, server *mcp.Server, store projectMemoryStore, err error) (*mcp.Server, error) {
	f.mu.Lock()
	if !f.closed {
		delete(f.initializing, project)
		if err == nil {
			f.projects[project] = projectEntry{server: server, store: store}
		}
		initialization.server = server
		initialization.err = err
		close(initialization.ready)
		f.mu.Unlock()
		return server, err
	}
	f.mu.Unlock()

	if !isNilProjectMemoryStore(store) {
		if closeErr := store.Close(); closeErr != nil {
			err = errors.Join(fmt.Errorf("%w", ErrProjectFactoryClosed), fmt.Errorf("close project memory: %w", closeErr))
		} else {
			err = fmt.Errorf("%w", ErrProjectFactoryClosed)
		}
	} else {
		err = fmt.Errorf("%w", ErrProjectFactoryClosed)
	}
	server = nil

	f.mu.Lock()
	delete(f.initializing, project)
	initialization.server = nil
	initialization.err = err
	close(initialization.ready)
	f.mu.Unlock()
	return nil, err
}

func isNilProjectMemoryStore(store projectMemoryStore) bool {
	if store == nil {
		return true
	}
	value := reflect.ValueOf(store)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (f *ProjectFactory) Close() error {
	f.mu.Lock()
	if f.closed {
		ready := f.closeReady
		f.mu.Unlock()
		<-ready
		f.mu.Lock()
		err := f.closeErr
		f.mu.Unlock()
		return err
	}
	f.closed = true
	initializations := make([]*projectInitialization, 0, len(f.initializing))
	for _, initialization := range f.initializing {
		initializations = append(initializations, initialization)
	}
	projects := make([]string, 0, len(f.projects))
	for project := range f.projects {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	stores := make([]projectMemoryStore, 0, len(projects))
	for _, project := range projects {
		stores = append(stores, f.projects[project].store)
	}
	f.mu.Unlock()

	for _, initialization := range initializations {
		<-initialization.ready
	}
	var closeErr error
	for _, store := range stores {
		if err := store.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close project memory: %w", err))
		}
	}
	f.mu.Lock()
	f.closeErr = closeErr
	close(f.closeReady)
	f.mu.Unlock()
	return closeErr
}

func ProjectHandler(servers ProjectServers) http.Handler {
	capacityServer := rejectionServer("project capacity reached")
	unavailableServer := rejectionServer("project service unavailable")
	streamable := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		project, _ := request.Context().Value(projectContextKey{}).(string)
		server, err := servers.Server(request.Context(), project)
		if errors.Is(err, ErrProjectCapacity) {
			return capacityServer
		}
		if err != nil || server == nil {
			return unavailableServer
		}
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		PropagateRequestCancellation: true,
		CrossOriginProtection:        http.NewCrossOriginProtection(),
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		project, ok := projectFromQuery(request.URL.RawQuery)
		if !ok {
			http.Error(response, "invalid project", http.StatusBadRequest)
			return
		}
		ctx := context.WithValue(request.Context(), projectContextKey{}, project)
		streamable.ServeHTTP(response, request.WithContext(ctx))
	}))
	mux.Handle("/attachments", attachmentHTTP(servers))
	mux.Handle("/attachments/", attachmentHTTP(servers))
	mux.HandleFunc("/healthz", healthHandler)
	return mux
}

func rejectionServer(message string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-session-broker", Version: "dev"}, nil)
	server.AddReceivingMiddleware(func(mcp.MethodHandler) mcp.MethodHandler {
		return func(context.Context, string, mcp.Request) (mcp.Result, error) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: message}
		}
	})
	return server
}

func projectFromQuery(rawQuery string) (string, bool) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", false
	}
	projects, ok := query["project"]
	if !ok || len(projects) != 1 || !projectPattern.MatchString(projects[0]) {
		return "", false
	}
	return projects[0], true
}
