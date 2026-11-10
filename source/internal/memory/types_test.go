package memory

import (
	"encoding/json"
	"testing"
)

func TestPutInputOmitsFalseArchived(t *testing.T) {
	encoded, err := json.Marshal(PutInput{Title: "decision", Body: "body", CreatedBy: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, found := fields["archived"]; found {
		t.Fatalf("archived must be omitted when false: %s", encoded)
	}
}
