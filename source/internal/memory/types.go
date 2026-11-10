package memory

import (
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("memory node not found")
	ErrInvalidInput = errors.New("invalid memory input")
)

const (
	MaxTitleRunes = 200
	MaxBodyBytes  = 64 * 1024
	MaxTags       = 32
	MaxLinks      = 64
	DefaultLimit  = 20
	MaxLimit      = 100
)

type PutInput struct {
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	CreatedBy  string   `json:"created_by"`
	Tags       []string `json:"tags,omitempty"`
	Links      []string `json:"links,omitempty"`
	Supersedes string   `json:"supersedes,omitempty"`
	Archived   bool     `json:"archived,omitempty"`
}

type Node struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	Tags       []string  `json:"tags,omitempty"`
	Links      []string  `json:"links,omitempty"`
	Supersedes string    `json:"supersedes,omitempty"`
	Archived   bool      `json:"archived"`
}

type GetResult struct {
	Node       Node     `json:"node"`
	Backlinks  []string `json:"backlinks"`
	ReplacedBy string   `json:"replaced_by,omitempty"`
}

type SearchInput struct {
	Query          string   `json:"query,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	LinkedTo       string   `json:"linked_to,omitempty"`
	CreatedBy      string   `json:"created_by,omitempty"`
	IncludeHistory bool     `json:"include_history,omitempty"`
	Limit          int      `json:"limit,omitempty"`
}

type Summary struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	CreatedBy  string    `json:"created_by"`
	Snippet    string    `json:"snippet"`
	CreatedAt  time.Time `json:"created_at"`
	Tags       []string  `json:"tags,omitempty"`
	Links      []string  `json:"links,omitempty"`
	Supersedes string    `json:"supersedes,omitempty"`
	Archived   bool      `json:"archived"`
}

type Warning struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

type SearchResult struct {
	Nodes    []Summary `json:"nodes"`
	Warnings []Warning `json:"warnings"`
}
