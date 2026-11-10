package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/pathguard"
)

var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type Store struct {
	root          string
	rootDirectory directoryPin
	directory     directoryPin
	project       ProjectDirectory
	lifecycle     sync.RWMutex
	closed        bool
	mu            sync.Mutex
	random        io.Reader
}

// ProjectDirectory is a pinned project-root capability. Implementations must
// expose a canonical path for identity checks and a handle-anchored operation
// path for child creation.
type ProjectDirectory interface {
	CanonicalPath() string
	OperationPath() string
	Validate() error
}

func NewStore(root string) (*Store, error) {
	return newStore(root, pfs.Link)
}

func newStore(root string, link linkFileFunc) (*Store, error) {
	return newStoreWithCreateHook(root, link, nil)
}

func newStoreWithCreateHook(root string, link linkFileFunc, hook pathguard.CreateHook) (*Store, error) {
	resolvedRoot, rootIdentity, rootDirectory, err := pathguard.EnsureDirectory(root, 0o700, openPathguardPin, hook)
	if err != nil {
		return nil, sanitizeStoreDirectoryError(err)
	}
	store, err := initializeStore(nil, resolvedRoot, rootIdentity, rootDirectory, link)
	if err != nil {
		_ = rootDirectory.Close()
		return nil, err
	}
	return store, nil
}

func openPathguardPin(canonicalPath, operationPath string, identity os.FileInfo) (pathguard.Pin, error) {
	return pathguard.NewPin(canonicalPath, operationPath, identity, pathguard.PinLabels{Store: "memory", Access: "nodes", Invalid: ErrInvalidInput})
}

// NewStoreInProject creates the fixed "memory" child only through a pinned
// Registry project capability, then proves that the pinned memory root and
// nodes directory still match their canonical identities inside that project.
func NewStoreInProject(project ProjectDirectory) (*Store, error) {
	return newStoreInProject(project, pfs.Link)
}

func newStoreInProject(project ProjectDirectory, link linkFileFunc) (*Store, error) {
	if project == nil {
		return nil, fmt.Errorf("%w: project directory is unavailable", ErrInvalidInput)
	}
	if err := project.Validate(); err != nil {
		return nil, fmt.Errorf("%w: validate project directory: %v", ErrInvalidInput, err)
	}
	resolvedRoot, rootIdentity, rootDirectory, exists, err := pathguard.EnsureChild(projectDirectoryAdapter{ProjectDirectory: project}, project.CanonicalPath(), "memory", true, 0o700, openPathguardPin, nil)
	if err != nil {
		return nil, sanitizeStoreDirectoryError(err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: project memory root is unavailable", ErrInvalidInput)
	}
	store, err := initializeStore(project, resolvedRoot, rootIdentity, rootDirectory, link)
	if err != nil {
		_ = rootDirectory.Close()
		return nil, err
	}
	return store, nil
}

type projectDirectoryAdapter struct {
	ProjectDirectory
}

func (directory projectDirectoryAdapter) Path() string { return directory.OperationPath() }

func initializeStore(project ProjectDirectory, resolvedRoot string, rootIdentity os.FileInfo, rootDirectory directoryPin, link linkFileFunc) (*Store, error) {
	if err := rootDirectory.Validate(); err != nil {
		return nil, fmt.Errorf("%w: validate pinned memory root: %v", ErrInvalidInput, err)
	}
	_, _, directory, exists, err := pathguard.EnsureChild(rootDirectory, resolvedRoot, "nodes", true, 0o700, openPathguardPin, nil)
	if err != nil {
		return nil, sanitizeStoreDirectoryError(err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: memory nodes directory is unavailable", ErrInvalidInput)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = directory.Close()
		}
	}()
	if project != nil {
		if err := project.Validate(); err != nil {
			return nil, fmt.Errorf("%w: validate project directory: %v", ErrInvalidInput, err)
		}
	}
	if err := rootDirectory.Validate(); err != nil {
		return nil, fmt.Errorf("%w: validate memory root identity: %v", ErrInvalidInput, err)
	}
	currentRoot, _, err := inspectDirectory(resolvedRoot)
	if err != nil || !os.SameFile(rootIdentity, currentRoot) {
		return nil, fmt.Errorf("%w: memory root identity changed during construction", ErrInvalidInput)
	}
	if err := directory.Validate(); err != nil {
		return nil, fmt.Errorf("%w: validate memory nodes identity: %v", ErrInvalidInput, err)
	}
	if err := preflightHardLinks(directory.Path(), link); err != nil {
		return nil, fmt.Errorf("initialize immutable memory commits: %w", err)
	}

	cleanup = false
	return &Store{
		root:          resolvedRoot,
		rootDirectory: rootDirectory,
		directory:     directory,
		project:       project,
		random:        rand.Reader,
	}, nil
}

func (s *Store) Root() string {
	return s.root
}

func (s *Store) Close() error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.directory.Close(), s.rootDirectory.Close())
}

func (s *Store) Put(ctx context.Context, input PutInput) (Node, error) {
	if err := access.RequireScope(ctx, access.MemoryWrite); err != nil {
		return Node{}, err
	}
	if err := s.beginOperation(); err != nil {
		return Node{}, err
	}
	defer s.endOperation()
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	if err := validateRawPutInput(input); err != nil {
		return Node{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	input = normalizePutInput(input)
	if err := validatePutInput(input); err != nil {
		return Node{}, err
	}
	nodes, corrupt, err := s.loadCompletedNodes(ctx)
	if err != nil {
		return Node{}, err
	}
	for _, target := range input.Links {
		if err := requireNode(target, nodes, corrupt); err != nil {
			return Node{}, fmt.Errorf("%w: link %q: %v", ErrInvalidInput, target, err)
		}
	}
	if input.Supersedes != "" {
		if err := requireNode(input.Supersedes, nodes, corrupt); err != nil {
			return Node{}, fmt.Errorf("%w: supersedes %q: %v", ErrInvalidInput, input.Supersedes, err)
		}
		for _, node := range nodes {
			if node.Supersedes == input.Supersedes {
				return Node{}, fmt.Errorf("%w: node %q already has successor %q", ErrInvalidInput, input.Supersedes, node.ID)
			}
		}
	}

	for {
		id, err := s.generateID()
		if err != nil {
			return Node{}, fmt.Errorf("generate memory id: %w", err)
		}
		finalPath := filepath.Join(s.directory.Path(), id+".md")

		node := Node{
			ID:         id,
			Title:      input.Title,
			Body:       input.Body,
			CreatedAt:  time.Now().UTC(),
			CreatedBy:  input.CreatedBy,
			Tags:       append([]string(nil), input.Tags...),
			Links:      append([]string(nil), input.Links...),
			Supersedes: input.Supersedes,
			Archived:   input.Archived,
		}
		if err := s.writeNode(finalPath, node); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return Node{}, err
		}
		return node, nil
	}
}

func (s *Store) Get(ctx context.Context, id string) (GetResult, error) {
	if err := access.RequireScope(ctx, access.MemoryRead); err != nil {
		return GetResult{}, err
	}
	if err := s.beginOperation(); err != nil {
		return GetResult{}, err
	}
	defer s.endOperation()
	if !validMemoryID(id) {
		return GetResult{}, fmt.Errorf("%w: invalid memory id", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return GetResult{}, err
	}

	nodes, corrupt, err := s.loadCompletedNodes(ctx)
	if err != nil {
		return GetResult{}, err
	}
	node, ok := nodes[id]
	if !ok {
		if decodeErr, corruptRequested := corrupt[id]; corruptRequested {
			return GetResult{}, decodeErr
		}
		return GetResult{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	result := GetResult{Node: node}
	for _, candidate := range nodes {
		for _, link := range candidate.Links {
			if link == id {
				result.Backlinks = append(result.Backlinks, candidate.ID)
				break
			}
		}
		if candidate.Supersedes == id {
			if result.ReplacedBy != "" {
				return GetResult{}, fmt.Errorf("%w: node %q has multiple direct successors", ErrInvalidInput, id)
			}
			result.ReplacedBy = candidate.ID
		}
	}
	sort.Strings(result.Backlinks)
	return result, nil
}

func (s *Store) Search(ctx context.Context, input SearchInput) (SearchResult, error) {
	if err := access.RequireScope(ctx, access.MemoryRead); err != nil {
		return SearchResult{}, err
	}
	if err := s.beginOperation(); err != nil {
		return SearchResult{}, err
	}
	defer s.endOperation()
	if err := ctx.Err(); err != nil {
		return SearchResult{}, err
	}

	normalized, limit, err := normalizeSearchInput(input)
	if err != nil {
		return SearchResult{}, err
	}
	nodes, _, warnings, err := s.scanCompletedNodes(ctx)
	if err != nil {
		return SearchResult{}, err
	}

	successors := make(map[string]bool)
	for _, node := range nodes {
		if node.Supersedes != "" {
			successors[node.Supersedes] = true
		}
	}

	result := SearchResult{
		Nodes:    make([]Summary, 0),
		Warnings: warnings,
	}
	for _, node := range nodes {
		if !normalized.IncludeHistory && (node.Archived || successors[node.ID]) {
			continue
		}
		if !matchesSearch(node, normalized) {
			continue
		}
		result.Nodes = append(result.Nodes, summarizeNode(node))
	}
	sort.Slice(result.Nodes, func(left, right int) bool {
		if result.Nodes[left].CreatedAt.Equal(result.Nodes[right].CreatedAt) {
			return result.Nodes[left].ID < result.Nodes[right].ID
		}
		return result.Nodes[left].CreatedAt.After(result.Nodes[right].CreatedAt)
	})
	if len(result.Nodes) > limit {
		result.Nodes = result.Nodes[:limit]
	}
	return result, nil
}

func (s *Store) generateID() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(s.random, value[:]); err != nil {
		return "", err
	}
	return "mem-" + hex.EncodeToString(value[:]), nil
}

func (s *Store) writeNode(finalPath string, node Node) error {
	if err := s.revalidateDirectories(); err != nil {
		return err
	}
	temporaryPath := filepath.Join(s.directory.Path(), "."+node.ID+".tmp")
	file, err := pfs.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary memory node: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if s.revalidateDirectories() == nil {
			_ = pfs.Remove(temporaryPath)
		}
	}()

	bounded := &boundedPersistedWriter{writer: file}
	if err := encodeNode(bounded, node); err != nil {
		return fmt.Errorf("encode memory node: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync memory node: %w", err)
	}
	err = file.Close()
	closed = true
	if err != nil {
		return fmt.Errorf("close memory node: %w", err)
	}
	if err := s.revalidateDirectories(); err != nil {
		return err
	}
	if err := commitNoReplace(temporaryPath, finalPath); err != nil {
		return fmt.Errorf("commit memory node: %w", err)
	}
	return nil
}

// commitNoReplace publishes a completed temporary file by creating a hard link.
// Unlike pfs.Rename, pfs.Link fails atomically when finalPath already exists on
// both Windows and Unix, so an immutable node can never be replaced at commit.
func commitNoReplace(temporaryPath, finalPath string) error {
	return pfs.Link(temporaryPath, finalPath)
}

func (s *Store) loadCompletedNodes(ctx context.Context) (map[string]Node, map[string]error, error) {
	nodes, corrupt, _, err := s.scanCompletedNodes(ctx)
	return nodes, corrupt, err
}

func (s *Store) scanCompletedNodes(ctx context.Context) (map[string]Node, map[string]error, []Warning, error) {
	if err := s.revalidateDirectories(); err != nil {
		return nil, nil, nil, err
	}
	entries, err := pfs.ReadDir(s.directory.Path())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read memory nodes directory: %w", err)
	}
	nodes := make(map[string]Node)
	corrupt := make(map[string]error)
	warnings := make([]Warning, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		id, ok := completedNodeID(entry.Name())
		if !ok {
			warnings = append(warnings, Warning{
				File:  entry.Name(),
				Error: fmt.Sprintf("%v: invalid memory node filename", ErrInvalidInput),
			})
			continue
		}
		path := filepath.Join(s.directory.Path(), entry.Name())
		pathInfo, err := pfs.Lstat(path)
		if err != nil {
			recordCorruptNode(corrupt, &warnings, id, entry.Name(), fmt.Errorf("inspect memory node %q: %w", id, err))
			continue
		}
		if !pathInfo.Mode().IsRegular() {
			recordCorruptNode(corrupt, &warnings, id, entry.Name(), fmt.Errorf("%w: memory node %q is not a regular file", ErrInvalidInput, id))
			continue
		}
		file, err := pfs.Open(path)
		if err != nil {
			recordCorruptNode(corrupt, &warnings, id, entry.Name(), fmt.Errorf("read memory node %q: %w", id, err))
			continue
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			recordCorruptNode(corrupt, &warnings, id, entry.Name(), fmt.Errorf("inspect opened memory node %q: %w", id, statErr))
			continue
		}
		if !os.SameFile(pathInfo, openedInfo) {
			_ = file.Close()
			recordCorruptNode(corrupt, &warnings, id, entry.Name(), fmt.Errorf("%w: memory node %q changed while opening", ErrInvalidInput, id))
			continue
		}
		if openedInfo.Size() > maxPersistedNodeBytes {
			_ = file.Close()
			recordCorruptNode(corrupt, &warnings, id, entry.Name(), fmt.Errorf("%w: memory node %q exceeds maximum of %d bytes", ErrInvalidInput, id, maxPersistedNodeBytes))
			continue
		}
		node, decodeErr := decodeNodeContext(ctx, file, openedInfo.Size())
		closeErr := file.Close()
		if decodeErr != nil {
			if errors.Is(decodeErr, context.Canceled) || errors.Is(decodeErr, context.DeadlineExceeded) {
				return nil, nil, nil, decodeErr
			}
			recordCorruptNode(corrupt, &warnings, id, entry.Name(), fmt.Errorf("decode memory node %q: %w", id, decodeErr))
			continue
		}
		if closeErr != nil {
			recordCorruptNode(corrupt, &warnings, id, entry.Name(), fmt.Errorf("close memory node %q: %w", id, closeErr))
			continue
		}
		if node.ID != id {
			recordCorruptNode(corrupt, &warnings, id, entry.Name(), fmt.Errorf("%w: persisted id %q does not match filename id %q", ErrInvalidInput, node.ID, id))
			continue
		}
		nodes[id] = node
	}
	if err := quarantineInvalidGraphNodes(ctx, nodes, corrupt, &warnings); err != nil {
		return nil, nil, nil, err
	}
	sort.Slice(warnings, func(left, right int) bool {
		return warnings[left].File < warnings[right].File
	})
	return nodes, corrupt, warnings, nil
}

func recordCorruptNode(corrupt map[string]error, warnings *[]Warning, id, file string, err error) {
	corrupt[id] = err
	*warnings = append(*warnings, Warning{File: file, Error: err.Error()})
}

func quarantineInvalidGraphNodes(ctx context.Context, nodes map[string]Node, corrupt map[string]error, warnings *[]Warning) error {
	knownCorrupt := make(map[string]bool, len(corrupt))
	for id := range corrupt {
		knownCorrupt[id] = true
	}
	for {
		invalid := make(map[string]error)
		successors := make(map[string][]string)
		for id, node := range nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			for _, target := range node.Links {
				_, targetLoaded := nodes[target]
				targetCorrupt := knownCorrupt[target]
				if !targetLoaded && !targetCorrupt {
					invalid[id] = fmt.Errorf("%w: persisted link target %q is unavailable", ErrInvalidInput, target)
					break
				}
			}
			if _, alreadyInvalid := invalid[id]; alreadyInvalid {
				continue
			}
			if node.Supersedes != "" {
				if _, ok := nodes[node.Supersedes]; !ok {
					if !knownCorrupt[node.Supersedes] {
						invalid[id] = fmt.Errorf("%w: persisted supersedes target %q is unavailable", ErrInvalidInput, node.Supersedes)
					}
				} else {
					successors[node.Supersedes] = append(successors[node.Supersedes], id)
				}
			}
		}
		for target, successorIDs := range successors {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(successorIDs) <= 1 {
				continue
			}
			for _, id := range successorIDs {
				invalid[id] = fmt.Errorf("%w: persisted node %q has multiple direct successors", ErrInvalidInput, target)
			}
		}
		cycleMembers, err := supersessionCycleMembers(ctx, nodes)
		if err != nil {
			return err
		}
		for id := range cycleMembers {
			invalid[id] = fmt.Errorf("%w: persisted supersedes relationship forms a cycle", ErrInvalidInput)
		}
		if len(invalid) == 0 {
			return nil
		}
		for id, err := range invalid {
			recordCorruptNode(corrupt, warnings, id, id+".md", err)
			delete(nodes, id)
		}
	}
}

func supersessionCycleMembers(ctx context.Context, nodes map[string]Node) (map[string]bool, error) {
	const (
		visiting = 1
		visited  = 2
	)
	state := make(map[string]uint8, len(nodes))
	cycleMembers := make(map[string]bool)
	for start := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if state[start] != 0 {
			continue
		}
		path := make([]string, 0)
		positions := make(map[string]int)
		current := start
		for current != "" && state[current] == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			node, ok := nodes[current]
			if !ok {
				break
			}
			state[current] = visiting
			positions[current] = len(path)
			path = append(path, current)
			current = node.Supersedes
		}
		if current != "" && state[current] == visiting {
			if cycleStart, ok := positions[current]; ok {
				for _, id := range path[cycleStart:] {
					cycleMembers[id] = true
				}
			}
		}
		for _, id := range path {
			state[id] = visited
		}
	}
	return cycleMembers, nil
}

func (s *Store) revalidateDirectories() error {
	if s.project != nil {
		if err := s.project.Validate(); err != nil {
			return fmt.Errorf("%w: pinned project directory unavailable: %v", ErrInvalidInput, err)
		}
	}
	if err := s.rootDirectory.Validate(); err != nil {
		return fmt.Errorf("%w: pinned memory root directory unavailable: %v", ErrInvalidInput, err)
	}
	if err := s.directory.Validate(); err != nil {
		return fmt.Errorf("%w: pinned memory nodes directory unavailable: %v", ErrInvalidInput, err)
	}
	return nil
}

func (s *Store) beginOperation() error {
	s.lifecycle.RLock()
	if s.closed {
		s.lifecycle.RUnlock()
		return fmt.Errorf("%w: memory store is closed", ErrInvalidInput)
	}
	return nil
}

func (s *Store) endOperation() {
	s.lifecycle.RUnlock()
}

func inspectDirectory(path string) (os.FileInfo, string, error) {
	return pathguard.InspectDirectory(path)
}

func validatePutInput(input PutInput) error {
	if input.Title == "" {
		return fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if utf8.RuneCountInString(input.Title) > MaxTitleRunes {
		return fmt.Errorf("%w: title exceeds %d runes", ErrInvalidInput, MaxTitleRunes)
	}
	if len(input.Body) > MaxBodyBytes {
		return fmt.Errorf("%w: body exceeds %d bytes", ErrInvalidInput, MaxBodyBytes)
	}
	if input.CreatedBy == "" {
		return fmt.Errorf("%w: created_by is required", ErrInvalidInput)
	}
	if len(input.Tags) > MaxTags {
		return fmt.Errorf("%w: too many tags: maximum is %d", ErrInvalidInput, MaxTags)
	}
	if len(input.Links) > MaxLinks {
		return fmt.Errorf("%w: too many links: maximum is %d", ErrInvalidInput, MaxLinks)
	}
	for _, tag := range input.Tags {
		if !tagPattern.MatchString(tag) {
			return fmt.Errorf("%w: invalid tag %q", ErrInvalidInput, tag)
		}
	}
	for _, id := range input.Links {
		if !validMemoryID(id) {
			return fmt.Errorf("%w: invalid link id %q", ErrInvalidInput, id)
		}
	}
	if input.Supersedes != "" && !validMemoryID(input.Supersedes) {
		return fmt.Errorf("%w: invalid supersedes id %q", ErrInvalidInput, input.Supersedes)
	}
	if input.Archived && input.Supersedes == "" {
		return fmt.Errorf("%w: archived node must supersede an existing node", ErrInvalidInput)
	}
	return nil
}

func validatePersistedNode(node Node) error {
	if !validMemoryID(node.ID) {
		return fmt.Errorf("%w: invalid persisted id", ErrInvalidInput)
	}
	input := PutInput{
		Title:      node.Title,
		Body:       node.Body,
		CreatedBy:  node.CreatedBy,
		Tags:       node.Tags,
		Links:      node.Links,
		Supersedes: node.Supersedes,
		Archived:   node.Archived,
	}
	if err := validateRawPutInput(input); err != nil {
		return fmt.Errorf("%w: invalid persisted metadata: %v", ErrInvalidInput, err)
	}
	if node.Title != strings.TrimSpace(node.Title) {
		return fmt.Errorf("%w: persisted title is not normalized", ErrInvalidInput)
	}
	if node.CreatedBy != strings.TrimSpace(node.CreatedBy) {
		return fmt.Errorf("%w: persisted created_by is not normalized", ErrInvalidInput)
	}
	if err := validatePutInput(input); err != nil {
		return fmt.Errorf("%w: invalid persisted metadata: %v", ErrInvalidInput, err)
	}
	normalized := normalizePutInput(input)
	if !sameStrings(node.Tags, normalized.Tags) {
		return fmt.Errorf("%w: persisted tags are not normalized and unique", ErrInvalidInput)
	}
	if !sameStrings(node.Links, normalized.Links) {
		return fmt.Errorf("%w: persisted links are not unique", ErrInvalidInput)
	}
	if containsString(node.Links, node.ID) {
		return fmt.Errorf("%w: persisted node links to itself", ErrInvalidInput)
	}
	if node.Supersedes == node.ID {
		return fmt.Errorf("%w: persisted node supersedes itself", ErrInvalidInput)
	}
	return nil
}

func validateRawPutInput(input PutInput) error {
	if len(input.Tags) > MaxTags {
		return fmt.Errorf("%w: too many tags: maximum is %d", ErrInvalidInput, MaxTags)
	}
	if len(input.Links) > MaxLinks {
		return fmt.Errorf("%w: too many links: maximum is %d", ErrInvalidInput, MaxLinks)
	}
	return nil
}

func normalizePutInput(input PutInput) PutInput {
	input.Title = strings.TrimSpace(input.Title)
	input.CreatedBy = strings.TrimSpace(input.CreatedBy)

	tags := make([]string, 0, len(input.Tags))
	seenTags := make(map[string]bool)
	for _, rawTag := range input.Tags {
		tag := strings.ToLower(strings.TrimSpace(rawTag))
		if !seenTags[tag] {
			seenTags[tag] = true
			tags = append(tags, tag)
		}
	}
	input.Tags = tags

	links := make([]string, 0, len(input.Links))
	seenLinks := make(map[string]bool)
	for _, link := range input.Links {
		if !seenLinks[link] {
			seenLinks[link] = true
			links = append(links, link)
		}
	}
	input.Links = links
	return input
}

func normalizeSearchInput(input SearchInput) (SearchInput, int, error) {
	if input.Limit < 0 {
		return SearchInput{}, 0, fmt.Errorf("%w: limit cannot be negative", ErrInvalidInput)
	}
	if len(input.Tags) > MaxTags {
		return SearchInput{}, 0, fmt.Errorf("%w: too many tags: maximum is %d", ErrInvalidInput, MaxTags)
	}
	if input.LinkedTo != "" && !validMemoryID(input.LinkedTo) {
		return SearchInput{}, 0, fmt.Errorf("%w: invalid linked_to id %q", ErrInvalidInput, input.LinkedTo)
	}

	tags := make([]string, 0, len(input.Tags))
	seen := make(map[string]bool)
	for _, rawTag := range input.Tags {
		tag := strings.ToLower(strings.TrimSpace(rawTag))
		if !tagPattern.MatchString(tag) {
			return SearchInput{}, 0, fmt.Errorf("%w: invalid tag %q", ErrInvalidInput, rawTag)
		}
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	if len(tags) > MaxTags {
		return SearchInput{}, 0, fmt.Errorf("%w: too many tags: maximum is %d", ErrInvalidInput, MaxTags)
	}
	input.Tags = tags

	limit := input.Limit
	if limit == 0 {
		limit = DefaultLimit
	} else if limit > MaxLimit {
		limit = MaxLimit
	}
	return input, limit, nil
}

func matchesSearch(node Node, input SearchInput) bool {
	if input.Query != "" {
		query := strings.ToLower(input.Query)
		if !strings.Contains(strings.ToLower(node.Title), query) && !strings.Contains(strings.ToLower(node.Body), query) {
			return false
		}
	}
	if input.CreatedBy != "" && node.CreatedBy != input.CreatedBy {
		return false
	}
	if input.LinkedTo != "" && !containsString(node.Links, input.LinkedTo) {
		return false
	}
	for _, tag := range input.Tags {
		if !containsString(node.Tags, tag) {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type boundedPersistedWriter struct {
	writer  io.Writer
	written int
}

func (w *boundedPersistedWriter) Write(content []byte) (int, error) {
	if len(content) > maxPersistedNodeBytes-w.written {
		return 0, fmt.Errorf("%w: persisted node exceeds maximum of %d bytes", ErrInvalidInput, maxPersistedNodeBytes)
	}
	count, err := w.writer.Write(content)
	w.written += count
	return count, err
}

func summarizeNode(node Node) Summary {
	return Summary{
		ID:         node.ID,
		Title:      node.Title,
		CreatedBy:  node.CreatedBy,
		Snippet:    makeSnippet(node.Body),
		CreatedAt:  node.CreatedAt,
		Tags:       append([]string(nil), node.Tags...),
		Links:      append([]string(nil), node.Links...),
		Supersedes: node.Supersedes,
		Archived:   node.Archived,
	}
}

func makeSnippet(body string) string {
	valid := strings.ToValidUTF8(body, "\uFFFD")
	singleLine := strings.Join(strings.Fields(valid), " ")
	runes := []rune(singleLine)
	if len(runes) > 240 {
		runes = runes[:240]
	}
	return string(runes)
}

func requireNode(id string, nodes map[string]Node, corrupt map[string]error) error {
	if _, ok := nodes[id]; ok {
		return nil
	}
	if err, ok := corrupt[id]; ok {
		return err
	}
	return ErrNotFound
}

func completedNodeID(name string) (string, bool) {
	if !strings.HasPrefix(name, "mem-") || !strings.HasSuffix(name, ".md") {
		return "", false
	}
	id := strings.TrimSuffix(name, ".md")
	return id, validMemoryID(id)
}

func sanitizeStoreDirectoryError(err error) error {
	if errors.Is(err, ErrInvalidInput) {
		return err
	}
	return fmt.Errorf("%w: memory directory is unsafe", ErrInvalidInput)
}
