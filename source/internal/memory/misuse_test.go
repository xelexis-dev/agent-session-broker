package memory

import "testing"

func TestMisuseA3GeneratedIDsStayInsideTheirProject(t *testing.T) {
	a, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// Empty body is allowed: title and author are required; IDs are generated.
	node, err := a.Put(t.Context(), PutInput{Title: "title only", CreatedBy: "worker"})
	if err != nil || node.ID == "" {
		t.Fatal("title-only memory rejected")
	}
	if _, err := b.Get(t.Context(), node.ID); err == nil {
		t.Fatal("another project read memory by ID")
	}
	for _, in := range []PutInput{
		{Title: "link", Body: "body", CreatedBy: "worker", Links: []string{node.ID}},
		{Title: "replace", Body: "body", CreatedBy: "worker", Supersedes: node.ID},
	} {
		if _, err := b.Put(t.Context(), in); err == nil {
			t.Fatal("foreign memory used as link or supersedes")
		}
	}
	result, err := b.Search(t.Context(), SearchInput{})
	if err != nil || len(result.Nodes) != 0 {
		t.Fatal("rejected writes changed another project")
	}
}
