package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMisuseD5CorruptNodeIsLocalAndSearchWarns(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	good, err := store.Put(t.Context(), PutInput{Title: "healthy", Body: "body", CreatedBy: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := store.Put(t.Context(), PutInput{Title: "damaged", Body: "body", CreatedBy: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(filepath.Join(root, "nodes", bad.ID+".md"), []byte("synthetic invalid markdown record"), 0600) != nil {
		t.Fatal("damage failed")
	}
	if _, err = store.Get(t.Context(), bad.ID); err == nil {
		t.Fatal("corrupt node accepted")
	}
	got, err := store.Get(t.Context(), good.ID)
	if err != nil || got.Node.ID != good.ID {
		t.Fatal("unrelated node affected")
	}
	search, err := store.Search(t.Context(), SearchInput{})
	if err != nil || len(search.Nodes) != 1 || len(search.Warnings) != 1 {
		t.Fatal("corruption was not isolated with warning")
	}
}

func TestMisuseD6PublicationFailureKeepsOldNodes(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.Put(t.Context(), PutInput{Title: "before", Body: "body", CreatedBy: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the destination makes the real hard-link publication fail
	// after writing and syncing a complete temporary node.
	err = store.writeNode(filepath.Join(root, "nodes"), Node{ID: "mem-00000000000000000000000000000000", Title: "after", Body: "body", CreatedBy: "worker", Supersedes: original.ID})
	if err == nil {
		t.Fatal("blocked publication reported success")
	}
	current, err := store.Get(t.Context(), original.ID)
	if err != nil || current.ReplacedBy != "" {
		t.Fatal("failed publish changed predecessor")
	}
	store.Close()
	reopened, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Search(t.Context(), SearchInput{IncludeHistory: true})
	if err != nil || len(rows.Nodes) != 1 || rows.Nodes[0].ID != original.ID {
		t.Fatal("failed publication left partial durable memory")
	}
}
