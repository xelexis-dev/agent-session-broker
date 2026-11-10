package mcpserver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
)

type closingRemote struct {
	*broker.RemoteService
	closes int
}

func (s *closingRemote) Close() error { s.closes++; return nil }

func TestManagedFactorySharesSessionBackendAndClosesIt(t *testing.T) {
	registry, err := groups.NewRegistry(filepath.Join(t.TempDir(), "projects"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	service := &closingRemote{RemoteService: broker.NewRemoteService()}
	calls := 0
	f := NewManagedProjectFactory(registry, func(directory groups.ProjectDirectory) (Backend, error) {
		calls++
		if err := directory.Validate(); err != nil {
			t.Fatal(err)
		}
		return service, nil
	})
	ctx := context.Background()
	if _, err = f.Server(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	backend, err := f.RemoteBackend(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if backend != service || calls != 1 {
		t.Fatal("panel and MCP use different backends")
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if service.closes != 1 {
		t.Fatalf("backend closed %d times", service.closes)
	}
	if _, err = f.RemoteBackend(ctx, "test"); !errors.Is(err, ErrProjectFactoryClosed) {
		t.Fatalf("backend after close: %v", err)
	}
}

func TestManagedFactoryDoesNotPublishFailedBackend(t *testing.T) {
	registry, err := groups.NewRegistry(filepath.Join(t.TempDir(), "projects"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	broken := &closingRemote{RemoteService: broker.NewRemoteService()}
	f := NewManagedProjectFactory(registry, func(groups.ProjectDirectory) (Backend, error) { return broken, errors.New("read failed") })
	defer f.Close()
	if _, err = f.Server(context.Background(), "test"); err == nil {
		t.Fatal("failed backend published")
	}
	if broken.closes != 1 {
		t.Fatal("partial backend leaked")
	}
}
