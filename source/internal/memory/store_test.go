package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
)

func TestNewStoreRejectsSuppliedRootSymlink(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	root := filepath.Join(parent, "memory")
	if err := os.Symlink(outside, root); err != nil {
		skipIfSymlinkPrivilegeUnavailable(t, err)
		t.Fatalf("create root symlink: %v", err)
	}

	store, err := NewStore(root)
	if store != nil {
		_ = store.Close()
		t.Fatalf("NewStore(symlink) store = %T, want nil", store)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewStore(symlink) error = %v, want ErrInvalidInput", err)
	}
}

func TestNewStoreRejectsSymlinkAncestorBeforeCreatingMissingRoot(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		skipIfSymlinkPrivilegeUnavailable(t, err)
		t.Fatalf("create ancestor symlink: %v", err)
	}

	store, err := NewStore(filepath.Join(alias, "missing", "memory"))
	if store != nil {
		_ = store.Close()
		t.Fatalf("NewStore(ancestor symlink) store = %T, want nil", store)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewStore(ancestor symlink) error = %v, want ErrInvalidInput", err)
	}
	entries, readErr := pfs.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("NewStore created descendants through ancestor symlink: %v", entries)
	}
}

func TestNewStoreInProjectUsesPinnedProjectAndRejectsMemoryAliases(t *testing.T) {
	base := t.TempDir()
	registry, err := groups.NewRegistry(filepath.Join(base, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	mes, err := registry.ProjectStorage(context.Background(), "mes")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStoreInProject(mes)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(mes.CanonicalPath(), "memory"); store.Root() != want {
		t.Fatalf("project Store root = %q, want %q", store.Root(), want)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	erp, err := registry.ProjectStorage(context.Background(), "erp")
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(erp.CanonicalPath(), "memory")
	if err := os.Symlink(filepath.Join(mes.CanonicalPath(), "memory"), alias); err != nil {
		skipIfSymlinkPrivilegeUnavailable(t, err)
		t.Fatalf("create cross-project memory alias: %v", err)
	}
	aliased, err := NewStoreInProject(erp)
	if aliased != nil {
		_ = aliased.Close()
		t.Fatalf("NewStoreInProject(alias) store = %T, want nil", aliased)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewStoreInProject(alias) error = %v, want ErrInvalidInput", err)
	}
}

func TestProjectStoreRejectsLinuxMemorySwapWithoutWritingOutside(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux permits the rename needed to exercise the post-pin swap")
	}
	base := t.TempDir()
	registry, err := groups.NewRegistry(filepath.Join(base, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	project, err := registry.ProjectStorage(context.Background(), "mes")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStoreInProject(project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	memoryPath := store.Root()
	displaced := filepath.Join(project.CanonicalPath(), "displaced-memory")
	outside := t.TempDir()
	if err := pfs.Rename(memoryPath, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, memoryPath); err != nil {
		t.Fatal(err)
	}

	_, err = store.Put(context.Background(), PutInput{Title: "blocked", Body: "body", CreatedBy: "codex"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Put() after project memory swap error = %v, want ErrInvalidInput", err)
	}
	entries, err := pfs.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("project Store wrote through replacement symlink: %v", entries)
	}
}

func TestProjectStoreRejectsLinuxProjectSwapWithoutWritingOutside(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux permits the project-directory rename needed to exercise the swap")
	}
	base := t.TempDir()
	projectsRoot := filepath.Join(base, "projects")
	registry, err := groups.NewRegistry(projectsRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	project, err := registry.ProjectStorage(context.Background(), "mes")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStoreInProject(project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	displaced := filepath.Join(projectsRoot, "displaced-project")
	outside := t.TempDir()
	if err := pfs.Rename(project.CanonicalPath(), displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, project.CanonicalPath()); err != nil {
		t.Fatal(err)
	}

	_, err = store.Put(context.Background(), PutInput{Title: "blocked", Body: "body", CreatedBy: "codex"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Put() after project swap error = %v, want ErrInvalidInput", err)
	}
	entries, err := pfs.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("project Store wrote through replacement project symlink: %v", entries)
	}
}

func TestMarkdownRoundTripNormalizesWikiLinks(t *testing.T) {
	want := Node{
		ID:        "mem-0123456789abcdef0123456789abcdef",
		Title:     "Constraint",
		Body:      "Use Windows.",
		CreatedAt: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
		CreatedBy: "codex-dev",
		Tags:      []string{"windows"},
		Links:     []string{"mem-fedcba9876543210fedcba9876543210"},
		Archived:  false,
	}

	var encoded bytes.Buffer
	if err := encodeNode(&encoded, want); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded.String(), `"[[mem-fedcba9876543210fedcba9876543210]]"`) {
		t.Fatalf("missing wikilink: %s", encoded.String())
	}

	got, err := decodeNode(strings.NewReader(encoded.String()))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmpNode(got, want); diff != "" {
		t.Fatal(diff)
	}
}

func TestMarkdownRoundTripPreservesFractionalSeconds(t *testing.T) {
	want := Node{
		ID:        "mem-0123456789abcdef0123456789abcdef",
		Title:     "Timestamp precision",
		Body:      "Retain nanoseconds.",
		CreatedAt: time.Date(2026, 8, 30, 10, 0, 0, 123456789, time.UTC),
		CreatedBy: "codex-dev",
	}

	var encoded bytes.Buffer
	if err := encodeNode(&encoded, want); err != nil {
		t.Fatal(err)
	}
	got, err := decodeNode(strings.NewReader(encoded.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("created_at = %s, want %s", got.CreatedAt, want.CreatedAt)
	}
}

func TestDecodeNodeRejectsAdditionalYAMLDocument(t *testing.T) {
	markdown := `---
id: mem-0123456789abcdef0123456789abcdef
title: First document
created_at: 2026-08-30T10:00:00Z
created_by: codex-dev
archived: false
---
id: mem-fedcba9876543210fedcba9876543210
---

body`

	_, err := decodeNode(strings.NewReader(markdown))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("decodeNode error = %v, want ErrInvalidInput", err)
	}
}

func TestDecodeNodeRejectsOversizeWithoutUnboundedRead(t *testing.T) {
	const maxPersistedBytes = int64(MaxBodyBytes + 64*1024)
	reader := &countingFillReader{remaining: maxPersistedBytes + 1024*1024}
	_, err := decodeNode(reader)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("decodeNode() error=%v, want ErrInvalidInput", err)
	}
	if reader.read > maxPersistedBytes+1 {
		t.Fatalf("decodeNode() consumed %d bytes, want at most %d", reader.read, maxPersistedBytes+1)
	}
}

func TestStorePersistsAcrossRestartAndDerivesGraph(t *testing.T) {
	root := t.TempDir()
	store := mustStore(t, root)
	first := mustPut(t, store, PutInput{
		Title:     "First",
		Body:      "body",
		CreatedBy: "claude",
	})
	second := mustPut(t, store, PutInput{
		Title:      "Second",
		Body:       "body",
		CreatedBy:  "codex",
		Links:      []string{first.ID},
		Supersedes: first.ID,
	})

	reopened := mustStore(t, root)
	got, err := reopened.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmpNode(got.Node, first); diff != "" {
		t.Fatal(diff)
	}
	if !slices.Equal(got.Backlinks, []string{second.ID}) || got.ReplacedBy != second.ID {
		t.Fatalf("graph=%+v", got)
	}
}

func TestStoreRejectsMissingGraphTargets(t *testing.T) {
	missingID := "mem-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := []struct {
		name  string
		input PutInput
	}{
		{
			name: "link",
			input: PutInput{
				Title:     "Dangling link",
				Body:      "body",
				CreatedBy: "codex",
				Links:     []string{missingID},
			},
		},
		{
			name: "supersedes",
			input: PutInput{
				Title:      "Dangling replacement",
				Body:       "body",
				CreatedBy:  "codex",
				Supersedes: missingID,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := mustStore(t, t.TempDir())
			_, err := store.Put(context.Background(), test.input)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Put() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestStoreRejectsSecondDirectSuccessor(t *testing.T) {
	store := mustStore(t, t.TempDir())
	first := mustPut(t, store, PutInput{Title: "First", Body: "body", CreatedBy: "claude"})
	mustPut(t, store, PutInput{Title: "Second", Body: "body", CreatedBy: "codex", Supersedes: first.ID})

	_, err := store.Put(context.Background(), PutInput{
		Title:      "Competing successor",
		Body:       "body",
		CreatedBy:  "codex",
		Supersedes: first.ID,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Put() error = %v, want ErrInvalidInput", err)
	}
}

func TestStoreRejectsPathLikeGetIDs(t *testing.T) {
	store := mustStore(t, t.TempDir())
	for _, id := range []string{
		"../mem-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"mem-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/child",
		"mem-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\\child",
	} {
		t.Run(id, func(t *testing.T) {
			_, err := store.Get(context.Background(), id)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Get(%q) error = %v, want ErrInvalidInput", id, err)
			}
		})
	}
}

func TestStoreRetriesIDCollisionWithoutOverwritingExistingNode(t *testing.T) {
	store := mustStore(t, t.TempDir())
	existing := Node{
		ID:        "mem-11111111111111111111111111111111",
		Title:     "Existing",
		Body:      "preserve me",
		CreatedAt: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
		CreatedBy: "claude",
	}
	var existingMarkdown bytes.Buffer
	if err := encodeNode(&existingMarkdown, existing); err != nil {
		t.Fatal(err)
	}
	existingPath := filepath.Join(store.Root(), "nodes", existing.ID+".md")
	if err := os.WriteFile(existingPath, existingMarkdown.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	store.random = bytes.NewReader(append(bytes.Repeat([]byte{0x11}, 16), bytes.Repeat([]byte{0x22}, 16)...))
	created := mustPut(t, store, PutInput{Title: "New", Body: "body", CreatedBy: "codex"})
	if created.ID != "mem-22222222222222222222222222222222" {
		t.Fatalf("created ID = %q, want collision retry ID", created.ID)
	}

	after, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, existingMarkdown.Bytes()) {
		t.Fatalf("existing node was overwritten:\n%s", after)
	}
}

func TestCommitNoReplacePreservesDestinationCreatedBeforeCommit(t *testing.T) {
	directory := t.TempDir()
	temporaryPath := filepath.Join(directory, ".pending.tmp")
	finalPath := filepath.Join(directory, "mem-11111111111111111111111111111111.md")
	if err := os.WriteFile(temporaryPath, []byte("new node"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []byte("existing immutable node")
	if err := os.WriteFile(finalPath, want, 0o600); err != nil {
		t.Fatal(err)
	}

	err := commitNoReplace(temporaryPath, finalPath)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("commitNoReplace() error = %v, want os.ErrExist", err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("destination changed: got %q want %q", got, want)
	}
}

func TestPreflightHardLinksCleansProbeFiles(t *testing.T) {
	directory := t.TempDir()
	if err := preflightHardLinks(directory, pfs.Link); err != nil {
		t.Fatal(err)
	}
	entries, err := pfs.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("hard-link probes remain: %v", entries)
	}
}

func TestPreflightHardLinksReportsUnsupportedFilesystem(t *testing.T) {
	wantErr := errors.New("hard links disabled")
	directory := t.TempDir()
	err := preflightHardLinks(directory, func(_, _ string) error { return wantErr })
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "hard-link commit unavailable") {
		t.Fatalf("preflightHardLinks() error = %v, want clear wrapped error", err)
	}
	entries, readErr := pfs.ReadDir(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed hard-link probes remain: %v", entries)
	}
}

func TestNewStoreReportsHardLinkPreflightFailureAndReleasesPin(t *testing.T) {
	requireSupportedStorePlatform(t)
	root := t.TempDir()
	wantErr := errors.New("hard links disabled")
	store, err := newStore(root, func(_, _ string) error { return wantErr })
	if store != nil || !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "initialize immutable memory commits") {
		t.Fatalf("newStore() = (%v, %v), want nil and clear wrapped preflight error", store, err)
	}
	nodesPath := filepath.Join(root, "nodes")
	entries, readErr := pfs.ReadDir(nodesPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed initialization left probes: %v", entries)
	}
	if err := pfs.Rename(nodesPath, filepath.Join(root, "moved-nodes")); err != nil {
		t.Fatalf("rename nodes after failed initialization: %v", err)
	}
}

func TestStoreGetIgnoresUnrelatedCorruptionButReportsRequestedCorruption(t *testing.T) {
	store := mustStore(t, t.TempDir())
	valid := mustPut(t, store, PutInput{Title: "Valid", Body: "body", CreatedBy: "claude"})
	corruptID := "mem-cccccccccccccccccccccccccccccccc"
	corruptPath := filepath.Join(store.Root(), "nodes", corruptID+".md")
	if err := os.WriteFile(corruptPath, []byte("not frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(context.Background(), valid.ID)
	if err != nil {
		t.Fatalf("Get(valid) error = %v", err)
	}
	if got.Node.ID != valid.ID {
		t.Fatalf("Get(valid) ID = %q, want %q", got.Node.ID, valid.ID)
	}

	_, err = store.Get(context.Background(), corruptID)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(corrupt) error = %v, want decode error", err)
	}
}

func TestSearchAndGetPreserveHealthySupersessionSuffixAfterCorruptAncestor(t *testing.T) {
	store := mustStore(t, t.TempDir())
	v1 := mustPut(t, store, PutInput{Title: "Fact", Body: "v1", CreatedBy: "claude"})
	v2 := mustPut(t, store, PutInput{Title: "Fact", Body: "v2", CreatedBy: "codex", Supersedes: v1.ID})
	v3 := mustPut(t, store, PutInput{Title: "Fact", Body: "v3", CreatedBy: "claude", Supersedes: v2.ID})
	v4 := mustPut(t, store, PutInput{Title: "Fact", Body: "v4", CreatedBy: "codex", Supersedes: v3.ID})
	if err := os.WriteFile(filepath.Join(store.Root(), "nodes", v1.ID+".md"), []byte("not frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}

	current, err := store.Search(context.Background(), SearchInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Nodes) != 1 || current.Nodes[0].ID != v4.ID {
		t.Fatalf("current nodes=%+v, want only %q", current.Nodes, v4.ID)
	}
	if len(current.Warnings) != 1 || current.Warnings[0].File != v1.ID+".md" {
		t.Fatalf("current warnings=%+v, want only corrupt ancestor %q", current.Warnings, v1.ID+".md")
	}

	history, err := store.Search(context.Background(), SearchInput{IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	wantHistory := map[string]bool{v2.ID: true, v3.ID: true, v4.ID: true}
	if len(history.Nodes) != len(wantHistory) {
		t.Fatalf("history nodes=%+v, want v2-v4", history.Nodes)
	}
	for _, node := range history.Nodes {
		if !wantHistory[node.ID] {
			t.Fatalf("history contains unexpected node %q", node.ID)
		}
	}

	got, err := store.Get(context.Background(), v4.ID)
	if err != nil {
		t.Fatalf("Get(v4) error=%v", err)
	}
	if got.Node.ID != v4.ID {
		t.Fatalf("Get(v4) ID=%q, want %q", got.Node.ID, v4.ID)
	}
}

func TestSearchPreservesHealthyNodeLinkedToCorruptTarget(t *testing.T) {
	store := mustStore(t, t.TempDir())
	target := mustPut(t, store, PutInput{Title: "Target", Body: "target", CreatedBy: "claude"})
	referencer := mustPut(t, store, PutInput{
		Title: "Reference", Body: "healthy", CreatedBy: "codex", Links: []string{target.ID},
	})
	if err := os.WriteFile(filepath.Join(store.Root(), "nodes", target.ID+".md"), []byte("not frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := store.Search(context.Background(), SearchInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].ID != referencer.ID {
		t.Fatalf("nodes=%+v, want linked healthy node %q", result.Nodes, referencer.ID)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].File != target.ID+".md" {
		t.Fatalf("warnings=%+v, want only corrupt target %q", result.Warnings, target.ID+".md")
	}
}

func TestStoreGetReturnsNotFoundForAbsentNode(t *testing.T) {
	store := mustStore(t, t.TempDir())
	_, err := store.Get(context.Background(), "mem-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestSearchDefaultsToLatestNonArchivedAndCanIncludeHistory(t *testing.T) {
	store := mustStore(t, t.TempDir())
	v1 := mustPut(t, store, PutInput{Title: "Windows fact", Body: "old", CreatedBy: "claude", Tags: []string{"windows"}})
	v2 := mustPut(t, store, PutInput{Title: "Windows fact", Body: "new", CreatedBy: "codex", Tags: []string{"windows"}, Supersedes: v1.ID})
	_ = mustPut(t, store, PutInput{Title: "Archived", Body: "gone", CreatedBy: "codex", Supersedes: v2.ID, Archived: true})

	current, err := store.Search(context.Background(), SearchInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Nodes) != 0 {
		t.Fatalf("current=%+v", current.Nodes)
	}
	history, err := store.Search(context.Background(), SearchInput{IncludeHistory: true})
	if err != nil || len(history.Nodes) != 3 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestSearchMatchesCaseInsensitiveQueryAcrossTitleAndBody(t *testing.T) {
	store := mustStore(t, t.TempDir())
	titleMatch := mustPut(t, store, PutInput{Title: "Windows Constraint", Body: "unrelated", CreatedBy: "claude"})
	bodyMatch := mustPut(t, store, PutInput{Title: "Other", Body: "Runs on WINDOWS hosts", CreatedBy: "codex"})
	_ = mustPut(t, store, PutInput{Title: "Other", Body: "linux only", CreatedBy: "codex"})

	result, err := store.Search(context.Background(), SearchInput{Query: "wInDoWs", IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{titleMatch.ID: true, bodyMatch.ID: true}
	if len(result.Nodes) != len(want) {
		t.Fatalf("matches=%+v", result.Nodes)
	}
	for _, node := range result.Nodes {
		if !want[node.ID] {
			t.Fatalf("unexpected query match %q", node.ID)
		}
	}
}

func TestSearchAppliesANDTagsOutgoingLinkAndExactCreatorFilters(t *testing.T) {
	store := mustStore(t, t.TempDir())
	target := mustPut(t, store, PutInput{Title: "Target", Body: "body", CreatedBy: "claude"})
	want := mustPut(t, store, PutInput{
		Title: "Match", Body: "body", CreatedBy: "Codex-A",
		Tags: []string{"windows", "process"}, Links: []string{target.ID},
	})
	_ = mustPut(t, store, PutInput{
		Title: "Missing tag", Body: "body", CreatedBy: "Codex-A",
		Tags: []string{"windows"}, Links: []string{target.ID},
	})
	_ = mustPut(t, store, PutInput{
		Title: "Wrong creator", Body: "body", CreatedBy: "codex-a",
		Tags: []string{"windows", "process"}, Links: []string{target.ID},
	})

	result, err := store.Search(context.Background(), SearchInput{
		Tags: []string{"WINDOWS", "process"}, LinkedTo: target.ID, CreatedBy: "Codex-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].ID != want.ID {
		t.Fatalf("filtered nodes=%+v, want %q", result.Nodes, want.ID)
	}
}

func TestSearchSortsNewestFirstThenIDAndCapsLimits(t *testing.T) {
	store := mustStore(t, t.TempDir())
	createdAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	for index := 0; index < MaxLimit+5; index++ {
		id := fmt.Sprintf("mem-%032x", index+1)
		writePersistedNode(t, store, Node{
			ID: id, Title: fmt.Sprintf("Node %03d", index), Body: "body",
			CreatedAt: createdAt.Add(time.Duration(index/2) * time.Second), CreatedBy: "codex",
		})
	}

	defaults, err := store.Search(context.Background(), SearchInput{IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(defaults.Nodes) != DefaultLimit {
		t.Fatalf("default count=%d, want %d", len(defaults.Nodes), DefaultLimit)
	}
	if defaults.Nodes[0].ID != fmt.Sprintf("mem-%032x", MaxLimit+5) ||
		defaults.Nodes[1].ID != fmt.Sprintf("mem-%032x", MaxLimit+3) ||
		defaults.Nodes[2].ID != fmt.Sprintf("mem-%032x", MaxLimit+4) {
		t.Fatalf("newest tie order=%q, %q, %q", defaults.Nodes[0].ID, defaults.Nodes[1].ID, defaults.Nodes[2].ID)
	}

	capped, err := store.Search(context.Background(), SearchInput{IncludeHistory: true, Limit: MaxLimit + 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Nodes) != MaxLimit {
		t.Fatalf("capped count=%d, want %d", len(capped.Nodes), MaxLimit)
	}

	one, err := store.Search(context.Background(), SearchInput{IncludeHistory: true, Limit: 1})
	if err != nil || len(one.Nodes) != 1 {
		t.Fatalf("limit one=%+v err=%v", one, err)
	}
}

func TestSearchSnippetIsSingleLineValidUTF8AndAtMost240Runes(t *testing.T) {
	store := mustStore(t, t.TempDir())
	body := "first\r\nsecond\n" + strings.Repeat("界", 238) + string([]byte{0xff}) + "tail"
	node := mustPut(t, store, PutInput{Title: "Snippet", Body: body, CreatedBy: "codex"})

	result, err := store.Search(context.Background(), SearchInput{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].ID != node.ID {
		t.Fatalf("nodes=%+v", result.Nodes)
	}
	snippet := result.Nodes[0].Snippet
	if strings.ContainsAny(snippet, "\r\n") {
		t.Fatalf("snippet contains a line break: %q", snippet)
	}
	if !utf8.ValidString(snippet) {
		t.Fatalf("snippet is not valid UTF-8: %q", snippet)
	}
	if count := utf8.RuneCountInString(snippet); count > 240 {
		t.Fatalf("snippet rune count=%d, want <= 240", count)
	}
}

func TestSearchReturnsSortedWarningsAndHealthyResultsForCorruptFiles(t *testing.T) {
	store := mustStore(t, t.TempDir())
	healthy := mustPut(t, store, PutInput{Title: "Healthy", Body: "body", CreatedBy: "claude"})
	nodesPath := filepath.Join(store.Root(), "nodes")
	if err := os.WriteFile(filepath.Join(nodesPath, "mem-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.md"), []byte("not frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodesPath, "invalid.md"), []byte("not a node"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := store.Search(context.Background(), SearchInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].ID != healthy.ID {
		t.Fatalf("healthy nodes=%+v", result.Nodes)
	}
	if len(result.Warnings) != 2 || result.Warnings[0].File != "invalid.md" || result.Warnings[1].File != "mem-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.md" {
		t.Fatalf("warnings=%+v", result.Warnings)
	}
	for _, warning := range result.Warnings {
		if warning.Error == "" {
			t.Fatalf("warning missing error: %+v", warning)
		}
	}
}

func TestSearchWarnsForSymlinkedNodeWithoutFollowingIt(t *testing.T) {
	parent := t.TempDir()
	store := mustStore(t, filepath.Join(parent, "memory"))
	id := "mem-dddddddddddddddddddddddddddddddd"
	outside := filepath.Join(parent, "outside.md")
	writeNodeFile(t, outside, Node{ID: id, Title: "Outside", Body: "outside", CreatedAt: time.Now().UTC(), CreatedBy: "claude"})
	link := filepath.Join(store.Root(), "nodes", id+".md")
	if err := os.Symlink(outside, link); err != nil {
		skipIfSymlinkPrivilegeUnavailable(t, err)
		t.Fatal(err)
	}

	result, err := store.Search(context.Background(), SearchInput{IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 0 || len(result.Warnings) != 1 || result.Warnings[0].File != id+".md" {
		t.Fatalf("result=%+v", result)
	}
}

func TestSearchHonorsCancellationAndClosedStore(t *testing.T) {
	store := mustStore(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Search(ctx, SearchInput{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Search() error=%v, want context.Canceled", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Search(context.Background(), SearchInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("closed Search() error=%v, want ErrInvalidInput", err)
	}
}

func TestConcurrentPutAndSearchCreatesUniqueCompleteNodes(t *testing.T) {
	store := mustStore(t, t.TempDir())
	const writers = 12
	ids := make(chan string, writers)
	errs := make(chan error, writers*2)
	var group sync.WaitGroup
	for index := 0; index < writers; index++ {
		group.Add(2)
		go func(index int) {
			defer group.Done()
			node, err := store.Put(context.Background(), PutInput{Title: fmt.Sprintf("Node %d", index), Body: "body", CreatedBy: "codex"})
			if err != nil {
				errs <- err
				return
			}
			ids <- node.ID
		}(index)
		go func() {
			defer group.Done()
			if _, err := store.Search(context.Background(), SearchInput{IncludeHistory: true, Limit: MaxLimit}); err != nil {
				errs <- err
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(ids)
	unique := make(map[string]bool)
	for id := range ids {
		if unique[id] {
			t.Fatalf("duplicate ID %q", id)
		}
		unique[id] = true
	}
	if len(unique) != writers {
		t.Fatalf("unique IDs=%d, want %d", len(unique), writers)
	}
	result, err := store.Search(context.Background(), SearchInput{IncludeHistory: true, Limit: MaxLimit})
	if err != nil || len(result.Nodes) != writers || len(result.Warnings) != 0 {
		t.Fatalf("final search=%+v err=%v", result, err)
	}
}

func TestPutNormalizesMetadataWithoutChangingBodyOrCallerInput(t *testing.T) {
	store := mustStore(t, t.TempDir())
	target := mustPut(t, store, PutInput{Title: "Target", Body: "body", CreatedBy: "claude"})
	tags := []string{" Windows ", "PROCESS", "WINDOWS"}
	links := make([]string, 3)
	for index := range links {
		links[index] = target.ID
	}
	input := PutInput{
		Title: "  Decision \t", Body: " \nraw\x00 body \n", CreatedBy: "\n codex ",
		Tags: tags, Links: links,
	}
	originalTags := append([]string(nil), input.Tags...)
	originalLinks := append([]string(nil), input.Links...)

	node, err := store.Put(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if node.Title != "Decision" || node.CreatedBy != "codex" || node.Body != input.Body {
		t.Fatalf("normalized scalar fields=%+v", node)
	}
	if !slices.Equal(node.Tags, []string{"windows", "process"}) {
		t.Fatalf("normalized tags=%v", node.Tags)
	}
	if !slices.Equal(node.Links, []string{target.ID}) {
		t.Fatalf("deduplicated links=%v", node.Links)
	}
	if !slices.Equal(input.Tags, originalTags) || !slices.Equal(input.Links, originalLinks) {
		t.Fatalf("Put changed caller input: tags=%v links=%v", input.Tags, input.Links)
	}
}

func TestPutRejectsHugeRawDuplicateSlices(t *testing.T) {
	const hugeCount = 100_000

	t.Run("tags", func(t *testing.T) {
		store := mustStore(t, t.TempDir())
		tags := make([]string, hugeCount)
		for index := range tags {
			tags[index] = "windows"
		}
		_, err := store.Put(context.Background(), PutInput{
			Title: "Too many raw tags", Body: "body", CreatedBy: "codex", Tags: tags,
		})
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "too many tags") {
			t.Fatalf("Put() error=%v, want raw tag limit error", err)
		}
	})

	t.Run("links", func(t *testing.T) {
		store := mustStore(t, t.TempDir())
		target := mustPut(t, store, PutInput{Title: "Target", Body: "body", CreatedBy: "claude"})
		links := make([]string, hugeCount)
		for index := range links {
			links[index] = target.ID
		}
		_, err := store.Put(context.Background(), PutInput{
			Title: "Too many raw links", Body: "body", CreatedBy: "codex", Links: links,
		})
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "too many links") {
			t.Fatalf("Put() error=%v, want raw link limit error", err)
		}
	})
}

func TestSearchRejectsHugeRawDuplicateTags(t *testing.T) {
	store := mustStore(t, t.TempDir())
	mustPut(t, store, PutInput{Title: "Tagged", Body: "body", CreatedBy: "codex", Tags: []string{"windows"}})
	tags := make([]string, 100_000)
	for index := range tags {
		tags[index] = "windows"
	}
	_, err := store.Search(context.Background(), SearchInput{Tags: tags})
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "too many tags") {
		t.Fatalf("Search() error=%v, want raw tag limit error", err)
	}
}

func TestPutRejectsWhitespaceOnlyRequiredFields(t *testing.T) {
	tests := []PutInput{
		{Title: " \t\n", Body: "body", CreatedBy: "codex"},
		{Title: "Title", Body: "body", CreatedBy: " \t\n"},
	}
	for _, input := range tests {
		store := mustStore(t, t.TempDir())
		if _, err := store.Put(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("Put(%+v) error=%v, want ErrInvalidInput", input, err)
		}
	}
}

func TestPutRejectsArchiveWithoutSupersedes(t *testing.T) {
	store := mustStore(t, t.TempDir())
	_, err := store.Put(context.Background(), PutInput{
		Title: "Archive without lineage", Body: "body", CreatedBy: "codex", Archived: true,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Put() error=%v, want ErrInvalidInput", err)
	}
	result, searchErr := store.Search(context.Background(), SearchInput{IncludeHistory: true})
	if searchErr != nil || len(result.Nodes) != 0 {
		t.Fatalf("rejected archive was persisted: result=%+v err=%v", result, searchErr)
	}
}

func TestPutLimitsRejectOverflow(t *testing.T) {
	tags := make([]string, MaxTags+1)
	for index := range tags {
		tags[index] = fmt.Sprintf("tag-%02d", index)
	}
	links := make([]string, MaxLinks+1)
	for index := range links {
		links[index] = fmt.Sprintf("mem-%032x", index+1000)
	}
	tests := []struct {
		name        string
		input       PutInput
		wantMessage string
	}{
		{
			name:        "title runes",
			input:       PutInput{Title: strings.Repeat("界", MaxTitleRunes+1), Body: "body", CreatedBy: "codex"},
			wantMessage: "title",
		},
		{
			name:        "body bytes",
			input:       PutInput{Title: "Title", Body: strings.Repeat("a", MaxBodyBytes+1), CreatedBy: "codex"},
			wantMessage: "body",
		},
		{
			name:        "tag count",
			input:       PutInput{Title: "Title", Body: "body", CreatedBy: "codex", Tags: tags},
			wantMessage: "too many tags",
		},
		{
			name:        "link count before target lookup",
			input:       PutInput{Title: "Title", Body: "body", CreatedBy: "codex", Links: links},
			wantMessage: "too many links",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := mustStore(t, t.TempDir())
			_, err := store.Put(context.Background(), test.input)
			if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("Put() error=%v, want ErrInvalidInput mentioning %q", err, test.wantMessage)
			}
		})
	}
}

func TestPutAcceptsExactTitleBodyAndTagLimits(t *testing.T) {
	tags := make([]string, MaxTags)
	for index := range tags {
		tags[index] = fmt.Sprintf("tag-%02d", index)
	}
	store := mustStore(t, t.TempDir())
	node, err := store.Put(context.Background(), PutInput{
		Title: strings.Repeat("界", MaxTitleRunes), Body: strings.Repeat("a", MaxBodyBytes),
		CreatedBy: "codex", Tags: tags,
	})
	if err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(node.Title) != MaxTitleRunes || len(node.Body) != MaxBodyBytes || len(node.Tags) != MaxTags {
		t.Fatalf("boundary node=%+v", node)
	}
}

func TestPutRejectsEncodedNodeBeyondPersistedLimit(t *testing.T) {
	store := mustStore(t, t.TempDir())
	_, err := store.Put(context.Background(), PutInput{
		Title: "Large creator", Body: strings.Repeat("x", MaxBodyBytes),
		CreatedBy: strings.Repeat("c", maxPersistedFrontmatterBytes),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Put() error=%v, want ErrInvalidInput", err)
	}
	result, searchErr := store.Search(context.Background(), SearchInput{IncludeHistory: true})
	if searchErr != nil || len(result.Nodes) != 0 {
		t.Fatalf("oversized node was persisted: result=%+v err=%v", result, searchErr)
	}
}

func TestPutCollisionCannotCreateSelfReference(t *testing.T) {
	store := mustStore(t, t.TempDir())
	existing := Node{
		ID: "mem-11111111111111111111111111111111", Title: "Existing", Body: "body",
		CreatedAt: time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC), CreatedBy: "claude",
	}
	writePersistedNode(t, store, existing)
	store.random = bytes.NewReader(append(bytes.Repeat([]byte{0x11}, 16), bytes.Repeat([]byte{0x22}, 16)...))

	created := mustPut(t, store, PutInput{
		Title: "Successor", Body: "body", CreatedBy: "codex",
		Links: []string{existing.ID}, Supersedes: existing.ID,
	})
	if created.ID != "mem-22222222222222222222222222222222" || created.Links[0] == created.ID || created.Supersedes == created.ID {
		t.Fatalf("created self-reference after collision: %+v", created)
	}
}

func TestSearchRejectsInvalidFiltersAndNegativeLimit(t *testing.T) {
	tests := []struct {
		name  string
		input SearchInput
	}{
		{name: "negative limit", input: SearchInput{Limit: -1}},
		{name: "invalid linked_to", input: SearchInput{LinkedTo: "../memory"}},
		{name: "invalid tag", input: SearchInput{Tags: []string{"bad/tag"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := mustStore(t, t.TempDir())
			mustPut(t, store, PutInput{Title: "Existing", Body: "body", CreatedBy: "codex"})
			if _, err := store.Search(context.Background(), test.input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Search(%+v) error=%v, want ErrInvalidInput", test.input, err)
			}
		})
	}
}

func TestSearchChecksCancellationBetweenFileReads(t *testing.T) {
	store := mustStore(t, t.TempDir())
	writePersistedNode(t, store, Node{
		ID: "mem-11111111111111111111111111111111", Title: "First", Body: "body",
		CreatedAt: time.Now().UTC(), CreatedBy: "codex",
	})
	writePersistedNode(t, store, Node{
		ID: "mem-22222222222222222222222222222222", Title: "Second", Body: "body",
		CreatedAt: time.Now().UTC(), CreatedBy: "codex",
	})
	ctx := newCancelAfterChecks(2)
	if _, err := store.Search(ctx, SearchInput{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error=%v, want cancellation between reads", err)
	}
}

func TestSearchChecksCancellationDuringFileRead(t *testing.T) {
	store := mustStore(t, t.TempDir())
	writePersistedNode(t, store, Node{
		ID: "mem-11111111111111111111111111111111", Title: "Large valid node",
		Body: strings.Repeat("x", 40*1024), CreatedAt: time.Now().UTC(), CreatedBy: "codex",
	})
	ctx := newCancelAfterChecks(3)
	if _, err := store.Search(ctx, SearchInput{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error=%v, want cancellation during file read", err)
	}
}

func TestSearchAndGetRejectOversizedPersistedFileBeforeDecode(t *testing.T) {
	store := mustStore(t, t.TempDir())
	healthy := mustPut(t, store, PutInput{Title: "Healthy", Body: "body", CreatedBy: "codex"})
	oversizedID := "mem-ffffffffffffffffffffffffffffffff"
	oversizedPath := filepath.Join(store.Root(), "nodes", oversizedID+".md")
	file, err := pfs.OpenFile(oversizedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(int64(MaxBodyBytes + 64*1024 + 1)); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := store.Search(context.Background(), SearchInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].ID != healthy.ID {
		t.Fatalf("healthy nodes=%+v", result.Nodes)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].File != oversizedID+".md" || !strings.Contains(result.Warnings[0].Error, "exceeds maximum") {
		t.Fatalf("warnings=%+v", result.Warnings)
	}
	_, err = store.Get(context.Background(), oversizedID)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("Get(oversized) error=%v, want size error", err)
	}
}

func TestSearchWarnsForPersistedSemanticCorruption(t *testing.T) {
	store := mustStore(t, t.TempDir())
	createdAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	healthyID := "mem-00000000000000000000000000000000"
	writePersistedNode(t, store, Node{
		ID: healthyID, Title: "Healthy", Body: "body", CreatedAt: createdAt, CreatedBy: "codex",
	})

	tags := make([]string, MaxTags+1)
	for index := range tags {
		tags[index] = fmt.Sprintf("tag-%02d", index)
	}
	links := make([]string, MaxLinks+1)
	for index := range links {
		links[index] = healthyID
	}
	cases := []Node{
		{ID: "mem-00000000000000000000000000000001", Title: strings.Repeat("界", MaxTitleRunes+1), Body: "body", CreatedAt: createdAt, CreatedBy: "codex"},
		{ID: "mem-00000000000000000000000000000002", Title: "Body overflow", Body: strings.Repeat("x", MaxBodyBytes+1), CreatedAt: createdAt, CreatedBy: "codex"},
		{ID: "mem-00000000000000000000000000000003", Title: "Unnormalized tag", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Tags: []string{"Windows"}},
		{ID: "mem-00000000000000000000000000000004", Title: "Too many tags", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Tags: tags},
		{ID: "mem-00000000000000000000000000000005", Title: "Duplicate links", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Links: []string{healthyID, healthyID}},
		{ID: "mem-00000000000000000000000000000006", Title: "Too many links", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Links: links},
		{ID: "mem-00000000000000000000000000000007", Title: "Unnormalized creator", Body: "body", CreatedAt: createdAt, CreatedBy: " codex "},
		{ID: "mem-00000000000000000000000000000008", Title: "Self supersedes", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Supersedes: "mem-00000000000000000000000000000008"},
		{ID: "mem-00000000000000000000000000000009", Title: "Archive without lineage", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Archived: true},
	}
	for _, node := range cases {
		writePersistedNode(t, store, node)
	}
	invalidIDFile := "mem-0000000000000000000000000000000a"
	writeNodeFile(t, filepath.Join(store.Root(), "nodes", invalidIDFile+".md"), Node{
		ID: "invalid", Title: "Invalid ID", Body: "body", CreatedAt: createdAt, CreatedBy: "codex",
	})

	result, err := store.Search(context.Background(), SearchInput{IncludeHistory: true, Limit: MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].ID != healthyID {
		t.Fatalf("healthy nodes=%+v", result.Nodes)
	}
	if len(result.Warnings) != len(cases)+1 {
		t.Fatalf("warnings=%+v", result.Warnings)
	}
	for index, warning := range result.Warnings {
		wantFile := fmt.Sprintf("mem-%032x.md", index+1)
		if warning.File != wantFile || warning.Error == "" {
			t.Fatalf("warning[%d]=%+v, want file %q with error", index, warning, wantFile)
		}
	}
	_, err = store.Get(context.Background(), cases[1].ID)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Get(semantic corruption) error=%v, want ErrInvalidInput", err)
	}
}

func TestSearchWarnsForPersistedDanglingGraphTargets(t *testing.T) {
	store := mustStore(t, t.TempDir())
	createdAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	healthyID := "mem-00000000000000000000000000000000"
	missingID := "mem-ffffffffffffffffffffffffffffffff"
	writePersistedNode(t, store, Node{ID: healthyID, Title: "Healthy", Body: "body", CreatedAt: createdAt, CreatedBy: "codex"})
	writePersistedNode(t, store, Node{
		ID: "mem-00000000000000000000000000000001", Title: "Dangling link", Body: "body",
		CreatedAt: createdAt, CreatedBy: "codex", Links: []string{missingID},
	})
	writePersistedNode(t, store, Node{
		ID: "mem-00000000000000000000000000000002", Title: "Dangling successor", Body: "body",
		CreatedAt: createdAt, CreatedBy: "codex", Supersedes: missingID,
	})

	result, err := store.Search(context.Background(), SearchInput{IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].ID != healthyID || len(result.Warnings) != 2 {
		t.Fatalf("result=%+v", result)
	}
	if result.Warnings[0].File != "mem-00000000000000000000000000000001.md" || result.Warnings[1].File != "mem-00000000000000000000000000000002.md" {
		t.Fatalf("warnings not sorted: %+v", result.Warnings)
	}
	_, err = store.Get(context.Background(), "mem-00000000000000000000000000000001")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Get(dangling) error=%v, want ErrInvalidInput", err)
	}
}

func TestSearchQuarantinesEntirePersistedDanglingSupersessionChain(t *testing.T) {
	store := mustStore(t, t.TempDir())
	createdAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	healthyID := "mem-00000000000000000000000000000000"
	firstID := "mem-00000000000000000000000000000001"
	secondID := "mem-00000000000000000000000000000002"
	missingID := "mem-ffffffffffffffffffffffffffffffff"
	writePersistedNode(t, store, Node{ID: healthyID, Title: "Healthy", Body: "body", CreatedAt: createdAt, CreatedBy: "codex"})
	writePersistedNode(t, store, Node{
		ID: firstID, Title: "Directly dangling", Body: "body", CreatedAt: createdAt,
		CreatedBy: "codex", Supersedes: missingID,
	})
	writePersistedNode(t, store, Node{
		ID: secondID, Title: "Transitively dangling", Body: "body", CreatedAt: createdAt,
		CreatedBy: "codex", Supersedes: firstID,
	})

	result, err := store.Search(context.Background(), SearchInput{IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].ID != healthyID {
		t.Fatalf("nodes=%+v, want only healthy node %q", result.Nodes, healthyID)
	}
	wantWarningFiles := []string{firstID + ".md", secondID + ".md"}
	if len(result.Warnings) != len(wantWarningFiles) {
		t.Fatalf("warnings=%+v, want dangling chain warnings", result.Warnings)
	}
	for index, wantFile := range wantWarningFiles {
		if result.Warnings[index].File != wantFile {
			t.Fatalf("warning[%d]=%+v, want file %q", index, result.Warnings[index], wantFile)
		}
	}
}

func TestSearchWarnsForCompetingAndCyclicPersistedSuccessors(t *testing.T) {
	store := mustStore(t, t.TempDir())
	createdAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	rootID := "mem-00000000000000000000000000000000"
	firstSuccessor := "mem-00000000000000000000000000000001"
	secondSuccessor := "mem-00000000000000000000000000000002"
	cycleA := "mem-00000000000000000000000000000003"
	cycleB := "mem-00000000000000000000000000000004"
	writePersistedNode(t, store, Node{ID: rootID, Title: "Healthy root", Body: "body", CreatedAt: createdAt, CreatedBy: "codex"})
	writePersistedNode(t, store, Node{ID: firstSuccessor, Title: "First successor", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Supersedes: rootID})
	writePersistedNode(t, store, Node{ID: secondSuccessor, Title: "Second successor", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Supersedes: rootID})
	writePersistedNode(t, store, Node{ID: cycleA, Title: "Cycle A", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Supersedes: cycleB})
	writePersistedNode(t, store, Node{ID: cycleB, Title: "Cycle B", Body: "body", CreatedAt: createdAt, CreatedBy: "codex", Supersedes: cycleA})

	result, err := store.Search(context.Background(), SearchInput{IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].ID != rootID || len(result.Warnings) != 4 {
		t.Fatalf("result=%+v", result)
	}
	for index, warning := range result.Warnings {
		wantFile := fmt.Sprintf("mem-%032x.md", index+1)
		if warning.File != wantFile {
			t.Fatalf("warning[%d]=%+v, want %q", index, warning, wantFile)
		}
	}
	_, err = store.Get(context.Background(), firstSuccessor)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Get(competing successor) error=%v, want ErrInvalidInput", err)
	}
}

func TestStorePinnedNodesDirectoryCannotBeRedirected(t *testing.T) {
	operations := []struct {
		name string
		run  func(*Store, string) error
	}{
		{
			name: "get",
			run: func(store *Store, existingID string) error {
				_, err := store.Get(context.Background(), existingID)
				return err
			},
		},
		{
			name: "put",
			run: func(store *Store, _ string) error {
				_, err := store.Put(context.Background(), PutInput{Title: "Redirected", Body: "body", CreatedBy: "codex"})
				return err
			},
		},
		{
			name: "search",
			run: func(store *Store, existingID string) error {
				result, err := store.Search(context.Background(), SearchInput{})
				if err == nil && (len(result.Nodes) != 1 || result.Nodes[0].ID != existingID) {
					return fmt.Errorf("Search() nodes = %+v, want %q", result.Nodes, existingID)
				}
				return err
			},
		},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			store := mustStore(t, t.TempDir())
			existing := mustPut(t, store, PutInput{Title: "Existing", Body: "body", CreatedBy: "claude"})
			nodesPath := filepath.Join(store.Root(), "nodes")
			displacedPath := filepath.Join(store.Root(), "displaced-nodes")
			renameErr := pfs.Rename(nodesPath, displacedPath)
			if renameErr == nil {
				if err := pfs.Mkdir(nodesPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			err := operation.run(store, existing.ID)
			if renameErr != nil {
				if err != nil {
					t.Fatalf("operation after blocked replacement = %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("operation after directory replacement = %v, want ErrInvalidInput", err)
			}
			entries, readErr := pfs.ReadDir(nodesPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("replacement nodes directory was accessed: %v", entries)
			}
		})
	}
}

func TestStorePinnedNodesSurviveRootReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "memory")
	store := mustStore(t, root)
	existing := mustPut(t, store, PutInput{Title: "Existing", Body: "body", CreatedBy: "claude"})
	displacedRoot := filepath.Join(parent, "displaced-memory")
	renameErr := pfs.Rename(root, displacedRoot)
	if renameErr == nil {
		if err := os.MkdirAll(filepath.Join(root, "nodes"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	_, err := store.Get(context.Background(), existing.ID)
	if renameErr != nil {
		if err != nil {
			t.Fatalf("Get() after blocked root replacement = %v", err)
		}
		return
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Get() after root replacement = %v, want ErrInvalidInput", err)
	}
}

func TestStorePinnedNodesRejectReplacementSymlink(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "memory")
	store := mustStore(t, root)
	existing := mustPut(t, store, PutInput{Title: "Existing", Body: "body", CreatedBy: "claude"})
	nodesPath := filepath.Join(store.Root(), "nodes")
	displacedPath := filepath.Join(parent, "displaced-nodes")
	outsidePath := filepath.Join(parent, "outside")
	if err := pfs.Rename(nodesPath, displacedPath); err != nil {
		if _, getErr := store.Get(context.Background(), existing.ID); getErr != nil {
			t.Fatalf("Get() after blocked nodes replacement = %v", getErr)
		}
		return
	}
	if err := pfs.Mkdir(outsidePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, nodesPath); err != nil {
		if restoreErr := pfs.Rename(displacedPath, nodesPath); restoreErr != nil {
			t.Fatalf("create symlink: %v; restore nodes: %v", err, restoreErr)
		}
		skipIfSymlinkPrivilegeUnavailable(t, err)
		t.Fatalf("create nodes symlink: %v", err)
	}

	_, err := store.Get(context.Background(), existing.ID)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Get() after replacement symlink error = %v, want ErrInvalidInput", err)
	}
	entries, err := pfs.ReadDir(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("replacement symlink target was accessed: %v", entries)
	}
}

func TestStoreRejectsSymlinkedNodeFile(t *testing.T) {
	parent := t.TempDir()
	store := mustStore(t, filepath.Join(parent, "memory"))
	id := "mem-dddddddddddddddddddddddddddddddd"
	node := Node{
		ID:        id,
		Title:     "Outside",
		Body:      "outside body",
		CreatedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		CreatedBy: "claude",
	}
	var encoded bytes.Buffer
	if err := encodeNode(&encoded, node); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(parent, "outside.md")
	if err := os.WriteFile(outsidePath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(store.Root(), "nodes", id+".md")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		skipIfSymlinkPrivilegeUnavailable(t, err)
		t.Fatalf("create node symlink: %v", err)
	}

	_, err := store.Get(context.Background(), id)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Get() error = %v, want ErrInvalidInput", err)
	}
}

func skipIfSymlinkPrivilegeUnavailable(t *testing.T, err error) {
	t.Helper()
	if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
		t.Skipf("symlink creation requires unavailable Windows privilege: %v", err)
	}
}

func TestStoreCloseReleasesPinnedDirectory(t *testing.T) {
	requireSupportedStorePlatform(t)
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if err := pfs.Rename(filepath.Join(store.Root(), "nodes"), filepath.Join(store.Root(), "moved-nodes")); err != nil {
		t.Fatalf("rename nodes after Close(): %v", err)
	}
	if err := pfs.Rename(store.Root(), store.Root()+"-moved"); err != nil {
		t.Fatalf("rename memory root after Close(): %v", err)
	}
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	value   byte
}

type countingFillReader struct {
	remaining int64
	read      int64
}

func (r *countingFillReader) Read(destination []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	count := int64(len(destination))
	if count > r.remaining {
		count = r.remaining
	}
	for index := int64(0); index < count; index++ {
		destination[index] = 'x'
	}
	r.remaining -= count
	r.read += count
	return int(count), nil
}

type cancelAfterChecks struct {
	mu        sync.Mutex
	remaining int
	done      chan struct{}
	once      sync.Once
}

func (c *cancelAfterChecks) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterChecks) Done() <-chan struct{}       { return c.done }
func (c *cancelAfterChecks) Value(any) any               { return nil }

func newCancelAfterChecks(remaining int) *cancelAfterChecks {
	return &cancelAfterChecks{remaining: remaining, done: make(chan struct{})}
}

func (c *cancelAfterChecks) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.remaining == 0 {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	c.remaining--
	return nil
}

func newBlockingReader(value byte) *blockingReader {
	return &blockingReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		value:   value,
	}
}

func (r *blockingReader) Read(destination []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	for i := range destination {
		destination[i] = r.value
	}
	return len(destination), nil
}

func mustStore(t *testing.T, root string) *Store {
	t.Helper()
	requireSupportedStorePlatform(t)
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close Store: %v", err)
		}
	})
	return store
}

func requireSupportedStorePlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("memory storage is unsupported on %s", runtime.GOOS)
	}
}

func mustPut(t *testing.T, store *Store, input PutInput) Node {
	t.Helper()
	node, err := store.Put(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func writePersistedNode(t *testing.T, store *Store, node Node) {
	t.Helper()
	writeNodeFile(t, filepath.Join(store.Root(), "nodes", node.ID+".md"), node)
}

func writeNodeFile(t *testing.T, path string, node Node) {
	t.Helper()
	var encoded bytes.Buffer
	if err := encodeNode(&encoded, node); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cmpNode(got, want Node) string {
	if got.ID != want.ID || got.Title != want.Title || got.Body != want.Body ||
		!got.CreatedAt.Equal(want.CreatedAt) || got.CreatedBy != want.CreatedBy ||
		!equalStrings(got.Tags, want.Tags) || !equalStrings(got.Links, want.Links) ||
		got.Supersedes != want.Supersedes || got.Archived != want.Archived {
		return fmt.Sprintf("decoded node differs from encoded node: got=%+v want=%+v", got, want)
	}
	return ""
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
