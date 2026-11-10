package protocol

import (
	"strings"
	"testing"
)

func TestLocalAttachmentEncodedBudget(t *testing.T) {
	for _, path := range []string{strings.Repeat("x", 1025), strings.Repeat("&", 400)} {
		out := LocalAttachment(&Attachment{ID: "hidden", Path: path, Size: 1, SHA256: strings.Repeat("a", 64)})
		if out.Error != "path-too-long" || out.Path != "" || out.ID != "" {
			t.Fatal("unsafe expanded projection accepted")
		}
	}
	original := Message{Body: "body", Attachment: &Attachment{ID: "hidden", Name: "private", Path: "/inbox/file", Size: 1, SHA256: "digest"}}
	projected := LocalMessage(original)
	if projected.Attachment.ID != "" || original.Attachment.ID != "hidden" || original.Body != "body" {
		t.Fatal("projection mutated original")
	}
	if len(projected.Body) <= len(original.Body) {
		t.Fatal("reminder missing")
	}
}
