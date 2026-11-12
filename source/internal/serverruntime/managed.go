package serverruntime

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

type panelBackend struct {
	factory    *mcpserver.ProjectFactory
	registry   *groups.Registry
	store      *controlpanel.Store
	bugreports *bugreport.Store
}

func (b panelBackend) BugReports() *bugreport.Store { return b.bugreports }

func (b panelBackend) DeleteProject(ctx context.Context, project string) error {
	if b.bugreports == nil {
		return b.deleteProjectGeneration(ctx, project, "")
	}
	if b.store == nil {
		return controlpanel.ErrForbidden
	}
	return b.DeleteProjectGeneration(ctx, project, b.store.ProjectGeneration(project))
}
func (b panelBackend) DeleteProjectGeneration(ctx context.Context, project, generation string) error {
	if b.store == nil || generation == "" || b.store.ProjectGeneration(project) != generation {
		return controlpanel.ErrForbidden
	}
	return b.deleteProjectGeneration(ctx, project, generation)
}
func (b panelBackend) deleteProjectGeneration(ctx context.Context, project, generation string) error {
	if err := b.factory.CloseProject(ctx, project); err != nil {
		return err
	}
	if b.bugreports != nil {
		if b.bugreports.IsUnavailable() {
			serverdiag.Emit(serverdiag.Event{Type: "project-deletion-warning", Reason: "unavailable-bugreport"})
		} else if err := b.bugreports.DeleteProject(project, generation); err != nil {
			return err
		}
	}
	return b.registry.DeleteProjectStorage(ctx, project)
}

func (b panelBackend) Sessions(ctx context.Context, project string) ([]controlpanel.Session, error) {
	return b.sessionRows(ctx, project, true)
}

// LiveSessions never performs the ordinary page's detached-row cleanup.
func (b panelBackend) LiveSessions(ctx context.Context, project string) ([]controlpanel.Session, error) {
	return b.sessionRows(ctx, project, false)
}
func (b panelBackend) sessionRows(ctx context.Context, project string, cleanup bool) ([]controlpanel.Session, error) {
	service, err := b.factory.RemoteBackend(ctx, project)
	if err != nil {
		return nil, err
	}
	sessions, err := service.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	runtimeInfo := map[string]protocol.RuntimeInfoSnapshot{}
	if source, ok := service.(interface {
		SessionRuntimeInfo() map[string]protocol.RuntimeInfoSnapshot
	}); ok {
		runtimeInfo = source.SessionRuntimeInfo()
	}
	owners := map[string]string{}
	if source, ok := service.(interface{ RegistrationOwners() map[string]string }); ok {
		owners = source.RegistrationOwners()
	}
	removed := map[string]bool{}
	if cleanup {
		if remover, ok := service.(interface {
			RemoveDetachedSessions(context.Context, []string) ([]string, error)
		}); ok {
			var selectors []string
			for _, session := range sessions {
				if session.ErrorCode == broker.CodeDetached {
					selectors = append(selectors, panelSessionSelector(session))
				}
			}
			if deleted, err := remover.RemoveDetachedSessions(ctx, selectors); err == nil {
				for _, selector := range deleted {
					removed[selector] = true
				}
			}
		}
	}
	result := make([]controlpanel.Session, 0, len(sessions))
	for _, s := range sessions {
		if removed[panelSessionSelector(s)] {
			continue
		}
		selector := s.ID
		if s.RegistrationID != "" {
			selector = s.RegistrationID
		}
		var info *protocol.RuntimeInfoSnapshot
		if value, ok := runtimeInfo[s.RegistrationID]; ok {
			info = &value
		}
		result = append(result, controlpanel.Session{RuntimeInfo: info, OwnerID: owners[s.RegistrationID], Activity: s.Activity, RegistrationID: s.RegistrationID, ID: selector, SessionID: s.ID, Name: s.Name, Harness: s.Harness, PID: s.PID, Status: string(s.Status), Ready: s.AutomaticDeliveryReady && s.Harness != "kimi", ClientID: s.ClientID, ProbePending: s.ProbePending, Suspended: s.Suspended, ErrorCode: s.ErrorCode})
	}
	return result, nil
}

func (b panelBackend) Detach(ctx context.Context, project, id string) error {
	service, err := b.factory.RemoteBackend(ctx, project)
	if err != nil {
		return err
	}
	lookup, err := panelDetachSelector(ctx, service, id)
	if err != nil {
		return err
	}
	session, err := service.GetSession(ctx, lookup)
	if err != nil {
		return err
	}
	selector := panelSessionSelector(session)
	detach := func() error { return service.StopSession(ctx, selector) }
	var detachErr error
	if session.RegistrationID == "" {
		detachErr = detach()
	} else {
		detachErr = b.panelManagement(ctx, service, project, selector, "detached", detach)
	}
	if err := detachErr; err != nil {
		return err
	}
	return b.removeDetached(ctx, service, project, session, true)
}

// Panel form ids are registration ids for durable rows and session ids for
// legacy rows. Resolve that contract before GetSession's bare-name precedence
// can select a different row. Explicit selectors keep their own namespace.
func panelDetachSelector(ctx context.Context, service mcpserver.Backend, id string) (string, error) {
	for _, prefix := range []string{broker.SelectorRegistration, broker.SelectorSession, broker.SelectorName} {
		if strings.HasPrefix(id, prefix) {
			return id, nil
		}
	}
	sessions, err := service.ListSessions(ctx)
	if err != nil {
		return "", err
	}
	for _, session := range sessions {
		if session.RegistrationID != "" && session.RegistrationID == id {
			return broker.SelectorRegistration + id, nil
		}
	}
	for _, session := range sessions {
		if session.RegistrationID == "" && session.ID == id {
			return broker.SelectorName + session.Name, nil
		}
	}
	// Non-panel callers may still supply a unique session id or a plain name.
	for _, session := range sessions {
		if session.ID == id {
			return broker.SelectorSession + id, nil
		}
	}
	for _, session := range sessions {
		if session.Name == id {
			return broker.SelectorName + id, nil
		}
	}
	return "", broker.ErrSessionNotFound
}

func panelSessionSelector(session broker.Session) string {
	if session.RegistrationID != "" {
		return broker.SelectorRegistration + session.RegistrationID
	}
	return broker.SelectorName + session.Name
}

func (b panelBackend) removeDetached(ctx context.Context, service mcpserver.Backend, project string, session broker.Session, revoke bool) error {
	remover, ok := service.(interface {
		RemoveDetachedSession(context.Context, string, func(broker.Session) error) error
	})
	if !ok {
		return errors.New("session removal unavailable")
	}
	err := remover.RemoveDetachedSession(ctx, panelSessionSelector(session), func(current broker.Session) error {
		if !revoke || b.store == nil {
			return nil
		}
		return b.store.RevokeSessionTokens(project, current.Name, bindingIdentity(current.Harness, current.ID, current.ClientID))
	})
	if err == nil && revoke && b.store != nil {
		if releaseErr := b.store.ReleaseDirectRegistration(project, bindingIdentity(session.Harness, session.ID, session.ClientID)); releaseErr != nil {
			return releaseErr
		}
	}
	// Another panel request may already have completed this cleanup.
	if errors.Is(err, broker.ErrSessionNotFound) {
		return nil
	}
	return err
}

func (b panelBackend) SetSuspended(ctx context.Context, project, id string, suspended bool) error {
	service, err := b.factory.RemoteBackend(ctx, project)
	if err != nil {
		return err
	}
	manager, ok := service.(interface {
		SetSuspended(context.Context, string, bool) error
	})
	if !ok {
		return errors.New("session suspension unavailable")
	}
	// UI identifiers for durable registrations are registration ids.
	kind := "unsuspended"
	if suspended {
		kind = "suspended"
	}
	return b.panelManagement(ctx, service, project, broker.SelectorRegistration+id, kind, func() error { return manager.SetSuspended(ctx, broker.SelectorRegistration+id, suspended) })
}

func (b panelBackend) SessionSuspended(ctx context.Context, project, nickname string) (bool, error) {
	service, err := b.factory.RemoteBackend(ctx, project)
	if err != nil {
		return false, err
	}
	session, err := service.GetSession(ctx, broker.SelectorName+nickname)
	if errors.Is(err, broker.ErrSessionNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return session.Suspended, nil
}

func newManagedHandler(directory string, newRemote func(groups.ProjectDirectory) (mcpserver.Backend, error), version ...string) (http.Handler, func() error, error) {
	return newManagedHandlerWithTrustedProxies(directory, newRemote, nil, version...)
}

func newManagedHandlerWithTrustedProxies(directory string, newRemote func(groups.ProjectDirectory) (mcpserver.Backend, error), proxies []netip.Prefix, version ...string) (http.Handler, func() error, error) {
	imports, err := existingProjects(filepath.Join(directory, "projects"))
	if err != nil {
		return nil, nil, err
	}
	store, err := controlpanel.OpenSQLite(filepath.Join(directory, "control"))
	if err != nil {
		return nil, nil, err
	}
	if !store.Ready() {
		serverdiag.Emit(serverdiag.Event{Type: "server-setup-required"})
	}
	if newRemote == nil {
		newRemote = func(directory groups.ProjectDirectory) (mcpserver.Backend, error) {
			return persistentBackendWithCleanup(directory, store)
		}
	}
	registry, err := groups.NewRegistry(filepath.Join(directory, "projects"))
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	reports, err := bugreport.Open(directory)
	if err != nil {
		if !errors.Is(err, sqlitestore.ErrCorrupt) {
			return nil, nil, errors.Join(err, registry.Close(), store.Close())
		}
		reports = bugreport.Unavailable()
	}
	spool, err := attachments.Open(directory)
	if err != nil {
		return nil, nil, errors.Join(err, reports.Close(), registry.Close(), store.Close())
	}
	store.SetAttachmentStore(spool)
	reports.SetProjectPolicy(store.BugReportPolicy)
	bindings := store.RegistrationOwnerBindings()
	retries := newOwnerBackfillRetries()
	factory := mcpserver.NewManagedProjectFactory(registry, func(directory groups.ProjectDirectory) (mcpserver.Backend, error) {
		service, err := newRemote(directory)
		if err != nil {
			return nil, err
		}
		if a, ok := service.(interface{ SetAttachments(*attachments.Store) }); ok {
			a.SetAttachments(spool)
		}
		if migration, ok := service.(ownerBackfiller); ok {
			name, decodeErr := hex.DecodeString(strings.TrimPrefix(filepath.Base(directory.CanonicalPath()), "p-"))
			if decodeErr != nil {
				return nil, errors.Join(decodeErr, closeOwnerMigrationBackend(service))
			}
			retries.run(migration, bindings[string(name)])
		}
		return service, nil
	}, version...)
	close := func() error {
		retries.close()
		return errors.Join(factory.Close(), spool.Close(), registry.Close(), store.Close(), reports.Close())
	}
	// Persist existing owner evidence before requests or retention housekeeping.
	for _, name := range imports {
		if _, err := factory.RemoteBackend(context.Background(), name); err != nil {
			// Keep verified project-local corruption isolated. Requests to this
			// project still fail through the factory; unrelated projects can start.
			if errors.Is(err, sqlitestore.ErrCorrupt) {
				continue
			}
			return nil, nil, errors.Join(err, close())
		}
	}

	if err := recoverManualRebinds(store, factory); err != nil {
		return nil, nil, errors.Join(err, close())
	}
	handler := controlpanel.NewWithTrustedProxies(store, panelBackend{factory: factory, registry: registry, store: store, bugreports: reports}, mcpserver.ProjectHandler(factory), proxies, version...)

	var importOnce sync.Once
	var importErr error
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store.Ready() {
			importOnce.Do(func() { importErr = store.ImportProjects(imports) })
			if importErr != nil {
				http.Error(w, "project migration unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		ctx := bugreport.WithActorName(bugreport.WithStore(r.Context(), reports), func(id string) string {
			if user, ok := store.User(id); ok {
				return user.Username
			}
			return ""
		})
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
	return wrapped, close, nil
}

func persistentBackend(directory groups.ProjectDirectory) (mcpserver.Backend, error) {
	return persistentBackendWithCleanup(directory, nil)
}
func persistentBackendWithCleanup(directory groups.ProjectDirectory, policy *controlpanel.Store) (mcpserver.Backend, error) {
	if err := directory.Validate(); err != nil {
		return nil, err
	}
	name, err := hex.DecodeString(strings.TrimPrefix(filepath.Base(directory.CanonicalPath()), "p-"))
	if err != nil {
		return nil, errors.New("invalid project storage identity")
	}
	var cleanup func([]access.DetachedRemovalProof) error
	if policy != nil {
		project := string(name)
		generation := policy.ProjectGeneration(project)
		cleanup = func(proofs []access.DetachedRemovalProof) error {
			return policy.CleanupDetachedAutoBatch(project, generation, proofs)
		}
	}
	remote, err := broker.NewSQLitePersistentRemoteServiceWithBatchCleanup(filepath.Join(directory.OperationPath(), "registrations"), cleanup)
	if err != nil {
		return nil, err
	}
	if err := directory.Validate(); err != nil {
		return nil, errors.Join(err, remote.Close())
	}
	return newProtectedBackend(string(name), remote), nil
}

// Existing project storage uses encoded directory names, including Windows
// reserved device names. Only canonical real directories qualify for import.
func existingProjects(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot inspect existing projects")
	}
	valid := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "p-") {
			continue
		}
		encoded := strings.TrimPrefix(entry.Name(), "p-")
		raw, err := hex.DecodeString(encoded)
		if err != nil || hex.EncodeToString(raw) != encoded || !valid.Match(raw) {
			continue
		}
		names = append(names, string(raw))
	}
	return names, nil
}

func closeOwnerMigrationBackend(service mcpserver.Backend) error {
	if closer, ok := service.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func (b panelBackend) panelManagement(ctx context.Context, service mcpserver.Backend, project, selector, kind string, fallback func() error) error {
	manager, ok := service.(interface {
		ManageSession(context.Context, string, string, broker.ManagementCheck, func(protocol.ManagementEvent) error) error
	})
	if !ok {
		return fallback()
	}
	audit := func(event protocol.ManagementEvent) error {
		if b.store != nil {
			return b.store.RecordSessionManagement(project, "panel", event)
		}
		return nil
	}
	return manager.ManageSession(ctx, selector, kind, func(broker.Session, string, []broker.Session) (protocol.ManagementEvent, error) {
		return protocol.ManagementEvent{ActorName: "panel"}, nil
	}, audit)
}
