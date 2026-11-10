package channel

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

// A conversation registered with a legacy session token keeps its
// registration when the same client switches to a project token: the resume
// secret and revision move into the new session state, the legacy file stays,
// and the broker accepts the registration instead of answering owned.
func TestProjectTokenClientAdoptsLegacyRegistrationOfTheSameConversation(t *testing.T) {
	upstream := newSwitchableUpstream(t)
	upstream.service.SetAdmission(grantAdmission{name: "agent-one"})
	stateDir := t.TempDir()
	registered := make(chan struct{})
	var registeredOnce sync.Once
	legacy := newDurableFixtureWithPolicy(t, upstream, stateDir, true, func(opts *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		opts.scope = "https://broker/mcp?project=brk"
		opts.bound = func(broker.Session) { registeredOnce.Do(func() { close(registered) }) }
	})
	legacy.attach(t, "agent-one")
	waitFor(t, "legacy registration", func() bool {
		s, err := upstream.session("agent-one")
		return err == nil && s.AutomaticDeliveryReady
	})
	// Server readiness precedes the register reply and the local save. Join
	// the post-save callback before shutdown so adoption resumes a committed
	// registration rather than replaying an unfinished register request.
	select {
	case <-registered:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the legacy registration to be saved locally")
	}
	original, _ := upstream.session("agent-one")
	legacy.stop()
	upstream.service.SetAdmission(nil)
	legacyFiles, _ := filepath.Glob(filepath.Join(stateDir, "registrations", "*.json"))
	if len(legacyFiles) != 1 {
		t.Fatalf("legacy state files: %v", legacyFiles)
	}
	before, _ := os.ReadFile(legacyFiles[0])

	auth := newDeviceAuthServer(t)
	f := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity())
	waitFor(t, "code request", func() bool { return f.codeRequests() == 1 })
	f.grant("11111111-1111-4111-8111-111111111111", "asb_s_one", "agent-one")
	waitFor(t, "registration with the adopted secret", func() bool {
		s, err := upstream.session("agent-one")
		return err == nil && s.AutomaticDeliveryReady && s.RegistrationID == original.RegistrationID && s.Revision > original.Revision
	})
	after, _ := os.ReadFile(legacyFiles[0])
	if string(before) != string(after) {
		t.Fatal("adoption changed the legacy registration file")
	}
	if files, _ := filepath.Glob(filepath.Join(stateDir, "sessions", "*", "conn.json")); len(files) != 1 {
		t.Fatalf("session state files: %v", files)
	}
}
