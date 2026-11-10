package memory

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"testing"
)

func TestStoreScopeBoundary(t *testing.T) {
	s, e := NewStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := access.WithGrant(t.Context(), access.Grant{Scopes: []string{access.SessionRead}})
	if _, e := s.Put(ctx, PutInput{Title: "denied", Body: "denied", CreatedBy: "caller"}); e != access.ErrInsufficientScope {
		t.Fatal("write bypass")
	}
	if _, e := s.Get(ctx, "missing"); e != access.ErrInsufficientScope {
		t.Fatal("read bypass")
	}
	if _, e := s.Search(ctx, SearchInput{Query: "denied"}); e != access.ErrInsufficientScope {
		t.Fatal("search bypass")
	}
	write := access.WithGrant(t.Context(), access.Grant{Scopes: []string{access.MemoryWrite}})
	node, e := s.Put(write, PutInput{Title: "allowed", Body: "scope boundary", CreatedBy: "caller"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(write, node.ID); e != access.ErrInsufficientScope {
		t.Fatal("write implies read")
	}
	read := access.WithGrant(t.Context(), access.Grant{Scopes: []string{access.MemoryRead}})
	got, e := s.Get(read, node.ID)
	if e != nil || got.Node.ID != node.ID {
		t.Fatal("read without write failed")
	}
	nodes, e := s.Search(t.Context(), SearchInput{Query: "denied"})
	if e != nil || len(nodes.Nodes) != 0 {
		t.Fatal("denied write changed memory")
	}
}
