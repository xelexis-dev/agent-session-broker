//go:build darwin || (linux && !android)

package memory

import (
	"context"
	"errors"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"path/filepath"
	"testing"
)

func TestPinnedNodesDirectoryRejectsRenameDuringPut(t *testing.T) {
	store := mustStore(t, t.TempDir())
	existing := mustPut(t, store, PutInput{Title: "Existing", Body: "body", CreatedBy: "claude"})
	reader := newBlockingReader(0x44)
	store.random = reader

	type putResult struct {
		node Node
		err  error
	}
	result := make(chan putResult, 1)
	go func() {
		node, err := store.Put(context.Background(), PutInput{Title: "Pinned", Body: "body", CreatedBy: "codex"})
		result <- putResult{node: node, err: err}
	}()
	<-reader.started

	nodesPath := filepath.Join(store.Root(), "nodes")
	movedPath := filepath.Join(store.Root(), "moved-nodes")
	if err := pfs.Rename(nodesPath, movedPath); err != nil {
		t.Fatal(err)
	}
	if err := pfs.Mkdir(nodesPath, 0o700); err != nil {
		t.Fatal(err)
	}
	close(reader.release)
	created := <-result
	if !errors.Is(created.err, ErrInvalidInput) {
		t.Fatalf("Put() error after nodes rename = %v, want ErrInvalidInput", created.err)
	}

	if created.node.ID != "" {
		t.Fatalf("Put() returned node %q after rejected swap", created.node.ID)
	}
	movedEntries, err := pfs.ReadDir(movedPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(movedEntries) != 1 || movedEntries[0].Name() != existing.ID+".md" {
		t.Fatalf("displaced directory changed after rejected Put: %v", movedEntries)
	}
	entries, err := pfs.ReadDir(nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("replacement nodes directory was accessed: %v", entries)
	}
}
