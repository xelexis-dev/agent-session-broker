package broker

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"strings"
	"testing"
	"time"
)

func TestBugReportManagementRequiresLiveLease(t *testing.T) {
	s := NewRemoteService()
	defer s.Close()
	req := RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/synthetic", ClientID: "fixture-pc", Name: "worker", ChannelID: "fixture-lease", Secret: strings.Repeat("s", 40), RequestID: "fixture-request"}
	result, err := s.RegisterSession(t.Context(), req)
	if err != nil || result.Code != "" {
		t.Fatal(err)
	}
	g := access.Grant{TokenID: "fixture", Project: "p", OwnerID: "admin", Check: func(identity string) error {
		var fields [3]string
		if json.Unmarshal([]byte(identity), &fields) != nil || fields[1] != req.SessionID {
			return ErrSessionIdentityUnauthorized
		}
		return nil
	}}
	ctx := access.WithGrant(t.Context(), g)
	if _, err = s.BugReportManagementSession(ctx); err != nil {
		t.Fatal(err)
	}
	now := s.now()
	s.now = func() time.Time { return now.Add(remoteLeaseDuration + time.Second) }
	if _, err = s.BugReportManagementSession(ctx); err == nil {
		t.Fatal("expired lease accepted")
	}
}
