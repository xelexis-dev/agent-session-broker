package controlpanel

import (
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A durable fixture consumed by the real managed runtime on both tested trees.
func TestMisuse5Y1Prepare300(t *testing.T) {
	root := os.Getenv("ASB_MISUSE_Y_ROOT")
	if root == "" {
		t.Skip("explicit owned temp directory required")
	}
	previousKDF := passwordKDF
	passwordKDF = productionPasswordKDF
	defer func() { passwordKDF = previousKDF }()
	s, e := OpenSQLite(filepath.Join(root, "control"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.Setup("misuse5-test-password") != nil || s.CreateProject("demo") != nil {
		t.Fatal("setup")
	}
	if e = s.SetScopeIssuanceReady("admin", true); e != nil {
		t.Fatal(e)
	}
	now := time.Now().Add(-15 * time.Minute)
	s.now = func() time.Time { return now }
	parent := directParent(t, s, 400)
	remote, e := broker.NewSQLitePersistentRemoteService(filepath.Join(root, "projects", "p-64656d6f", "registrations"))
	if e != nil {
		t.Fatal(e)
	}
	defer remote.Close()
	remote.SetAdmission(directAdmissionFixture{})
	for i := 1; i <= 300; i++ {
		now = now.Add(3 * time.Second)
		req := directRegisterRequest()
		req.SessionID = fmt.Sprintf("11111111-1111-4111-8111-%012d", i)
		req.ClientID = fmt.Sprint("scale-client-", i)
		req.RequestID = fmt.Sprint("scale-request-", i)
		req.ChannelID = fmt.Sprint("scale-lease-", i)
		req.Capabilities = []string{"registration.reattach-after-cleanup"}
		if i > 200 {
			remote.SetAdmission(nil)
			req.Name = fmt.Sprint("legacy-scale-", i)
			out, e := remote.RegisterSession(t.Context(), req)
			if e != nil || out.Code != "" {
				t.Fatal("legacy register", i, e, out.Code)
			}
			if e = remote.StopSession(t.Context(), out.Session.RegistrationID); e != nil {
				t.Fatal(e)
			}
			continue
		}
		form := directForm()
		form.Set("session_id", req.SessionID)
		form.Set("asb_client_instance_id", req.ClientID)
		form.Set("request_id", req.RequestID)
		form.Set("requested_nickname", fmt.Sprint("scale-", i))
		issued, e := s.ExchangeDirect("demo", parent, directRequest(form), DirectProof{Absent: true}, "fixture", false)
		if e != nil {
			t.Fatal("issue", i, e)
		}
		g := directGrant(t, s, issued.AccessToken)
		out, e := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
		if e != nil || out.Code != "" {
			t.Fatal("register", i, e, out.Code)
		}
		if e = confirmDirect(t, g, out); e != nil {
			t.Fatal("confirm", i, e)
		}
		if e = remote.StopSession(access.WithGrant(t.Context(), g), out.Session.RegistrationID); e != nil {
			t.Fatal(e)
		}
	}
	t.Log("200 genuine auto grants + 100 legacy detached registrations persisted; fixture clocks only bypass setup rate limits, not measured runtime")
}
