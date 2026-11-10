package mcpserver

import (
	"errors"
	"fmt"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/groups"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestAttachRejectSanitizesWrappedErrorsAndForgedGuides(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{fmt.Errorf("private detail: %w", groups.ErrSelectorConflict), "name-in-use"},
		{fmt.Errorf("private detail: %w", broker.ErrSessionNotFound), "invalid-identity"},
		{fmt.Errorf("private detail: %w", protocol.AttachRejection{Code: "capacity", Message: "synthetic-secret"}), "capacity"},
		{protocol.AttachRejection{Code: "synthetic-secret", Message: "synthetic-secret"}, "attach-rejected"},
		{errors.New("remote communication name is already registered /private/internal"), "attach-rejected"},
	} {
		if got := safeAttachRejection(tc.err); got != protocol.SafeAttachRejection(tc.code) {
			t.Fatalf("expected safe code %s, got %+v", tc.code, got)
		}
	}
}
