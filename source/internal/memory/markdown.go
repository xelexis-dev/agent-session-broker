package memory

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var memoryIDPattern = regexp.MustCompile(`^mem-[0-9a-f]{32}$`)

const (
	// The body limit is 64 KiB. A second 64 KiB envelope safely covers
	// encoded title, creator, timestamps, tags, links, and YAML overhead.
	maxPersistedFrontmatterBytes = 64 * 1024
	maxPersistedNodeBytes        = MaxBodyBytes + maxPersistedFrontmatterBytes
)

type frontmatter struct {
	ID         string    `yaml:"id"`
	Title      string    `yaml:"title"`
	CreatedAt  string    `yaml:"created_at"`
	CreatedBy  string    `yaml:"created_by"`
	Tags       []string  `yaml:"tags,omitempty"`
	Links      wikiLinks `yaml:"links,omitempty"`
	Supersedes string    `yaml:"supersedes,omitempty"`
	Archived   *bool     `yaml:"archived"`
}

type wikiLinks []string

func (links wikiLinks) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.SequenceNode}
	for _, id := range links {
		node.Content = append(node.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: "[[" + id + "]]",
			Style: yaml.DoubleQuotedStyle,
		})
	}
	return node, nil
}

func encodeNode(w io.Writer, node Node) error {
	if _, err := io.WriteString(w, "---\n"); err != nil {
		return err
	}

	encoder := yaml.NewEncoder(w)
	if err := encoder.Encode(frontmatter{
		ID:         node.ID,
		Title:      node.Title,
		CreatedAt:  node.CreatedAt.UTC().Format(time.RFC3339Nano),
		CreatedBy:  node.CreatedBy,
		Tags:       node.Tags,
		Links:      wikiLinks(node.Links),
		Supersedes: node.Supersedes,
		Archived:   boolPointer(node.Archived),
	}); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "---\n\n"); err != nil {
		return err
	}
	_, err := io.WriteString(w, node.Body)
	return err
}

func decodeNodeContext(ctx context.Context, r io.Reader, sizeHint int64) (Node, error) {
	content, err := readPersistedNode(ctx, r, sizeHint)
	if err != nil {
		return Node{}, err
	}
	if !bytes.HasPrefix(content, []byte("---\n")) {
		return Node{}, fmt.Errorf("%w: missing opening frontmatter delimiter", ErrInvalidInput)
	}

	remainder := content[len("---\n"):]
	separator := []byte("\n---\n\n")
	index := bytes.Index(remainder, separator)
	if index < 0 {
		return Node{}, fmt.Errorf("%w: missing closing frontmatter delimiter", ErrInvalidInput)
	}

	var fields frontmatter
	decoder := yaml.NewDecoder(bytes.NewReader(remainder[:index]))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fields); err != nil {
		return Node{}, fmt.Errorf("%w: decode frontmatter: %v", ErrInvalidInput, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return Node{}, fmt.Errorf("%w: multiple YAML documents: %v", ErrInvalidInput, err)
	}
	if fields.ID == "" || !validMemoryID(fields.ID) {
		return Node{}, fmt.Errorf("%w: invalid persisted id", ErrInvalidInput)
	}
	if fields.Title == "" || fields.CreatedBy == "" || fields.Archived == nil {
		return Node{}, fmt.Errorf("%w: missing required frontmatter field", ErrInvalidInput)
	}
	createdAt, err := time.Parse(time.RFC3339, fields.CreatedAt)
	if err != nil {
		return Node{}, fmt.Errorf("%w: invalid created_at: %v", ErrInvalidInput, err)
	}

	links := make([]string, len(fields.Links))
	for i, link := range fields.Links {
		id, ok := normalizeWikiLink(link)
		if !ok {
			return Node{}, fmt.Errorf("%w: invalid persisted link", ErrInvalidInput)
		}
		links[i] = id
	}
	if fields.Supersedes != "" && !validMemoryID(fields.Supersedes) {
		return Node{}, fmt.Errorf("%w: invalid persisted supersedes", ErrInvalidInput)
	}

	node := Node{
		ID:         fields.ID,
		Title:      fields.Title,
		Body:       string(remainder[index+len(separator):]),
		CreatedAt:  createdAt,
		CreatedBy:  fields.CreatedBy,
		Tags:       fields.Tags,
		Links:      links,
		Supersedes: fields.Supersedes,
		Archived:   *fields.Archived,
	}
	if err := validatePersistedNode(node); err != nil {
		return Node{}, err
	}
	return node, nil
}

func readPersistedNode(ctx context.Context, r io.Reader, sizeHint int64) ([]byte, error) {
	// Stat is only a capacity hint; a file may shrink or grow before Read.
	capacity := int(min(max(sizeHint, 0), int64(maxPersistedNodeBytes))) + 1
	content := make([]byte, 0, capacity)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(content) == cap(content) {
			capacity := min(2*cap(content), maxPersistedNodeBytes+1)
			grown := make([]byte, len(content), capacity)
			copy(grown, content)
			content = grown
		}
		count, readErr := r.Read(content[len(content):cap(content)])
		if count > 0 {
			content = content[:len(content)+count]
			if len(content) > maxPersistedNodeBytes {
				return nil, fmt.Errorf("%w: persisted node exceeds maximum of %d bytes", ErrInvalidInput, maxPersistedNodeBytes)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if readErr == io.EOF {
			return content, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		if count == 0 {
			return nil, io.ErrNoProgress
		}
	}
}

func validMemoryID(id string) bool {
	return memoryIDPattern.MatchString(id)
}

func normalizeWikiLink(link string) (string, bool) {
	if !strings.HasPrefix(link, "[[") || !strings.HasSuffix(link, "]]") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(link, "[["), "]]")
	return id, validMemoryID(id)
}

func boolPointer(value bool) *bool {
	return &value
}
