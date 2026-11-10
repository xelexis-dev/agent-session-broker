//go:build windows

package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedNodesDirectoryBlocksRenameDuringPut(t *testing.T) {
	store := mustStore(t, t.TempDir())
	reader := newBlockingReader(0x44)
	store.random = reader

	result := make(chan error, 1)
	go func() {
		_, err := store.Put(context.Background(), PutInput{Title: "Pinned", Body: "body", CreatedBy: "codex"})
		result <- err
	}()
	<-reader.started

	nodesPath := filepath.Join(store.Root(), "nodes")
	movedPath := filepath.Join(store.Root(), "moved-nodes")
	renameErr := os.Rename(nodesPath, movedPath)
	close(reader.release)
	putErr := <-result
	if renameErr == nil {
		if restoreErr := os.Rename(movedPath, nodesPath); restoreErr != nil {
			t.Fatalf("nodes rename unexpectedly succeeded; restore failed: %v", restoreErr)
		}
		t.Fatal("nodes rename unexpectedly succeeded while Put held a pinned directory")
	}
	if putErr != nil {
		t.Fatalf("Put() error after blocked rename = %v", putErr)
	}
}
