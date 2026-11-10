package channel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

func TestRejectedAuthStatusExplainsReapprovalWithoutSecrets(t *testing.T) {
	auth := newDeviceAuthServer(t)
	f := newDeviceModeFixture(t, auth, newSwitchableUpstream(t), t.TempDir(), singleIdentity())
	waitFor(t, "approval request", func() bool { return f.codeRequests() == 1 })
	f.grant("11111111-1111-4111-8111-111111111111", "asb_s_fixture", "agent-one")
	waitFor(t, "authorized", func() bool { return f.status("")["registered"] == true })
	f.mu.Lock()
	f.upstreams[0].rejected.Store(true)
	f.mu.Unlock()
	waitFor(t, "rejection", func() bool { return f.status("")["status"] == "rejected" })
	message, _ := f.status("")["message"].(string)
	if !strings.Contains(message, "attach_session") || !strings.Contains(message, "revoked") || !strings.Contains(message, "expired") {
		t.Fatalf("rejected auth_status lacks cause or reapproval step: %q", message)
	}
	if strings.Contains(message, "asb_s_fixture") || strings.Contains(message, "http") {
		t.Fatal("rejected guidance exposed credential or URL")
	}
}

func TestAuthorizationMessagesExposeOnlySafeApprovalLink(t *testing.T) {
	for _, state := range []string{clientstate.DeviceFailed, clientstate.DevicePending, clientstate.DeviceExpired, clientstate.DeviceDenied} {
		t.Run(state, func(t *testing.T) {
			a := &sessionAuth{state: clientstate.SessionState{Registration: clientstate.Registration{InitialAttempt: initialPending}, Device: &clientstate.DeviceAuthorization{Status: state, Error: "fixture-raw-secret https://invalid.example/?token=fixture", VerificationURI: "https://invalid.example/?token=fixture", UserCode: "FIXTURE-CODE", ExpiresAt: time.Now().Add(time.Hour)}}}
			a.deriveStatus(time.Now())
			for _, message := range []string{a.pendingText(), a.report(false, "", "").Message} {
				if strings.Contains(message, "fixture-raw-secret") || strings.Contains(message, "token=fixture") || (state != clientstate.DevicePending && (strings.Contains(message, "https://") || strings.Contains(message, "FIXTURE-CODE"))) {
					t.Fatal("authorization guidance leaked dynamic fields")
				}
				if !strings.Contains(message, "attach_session") && !strings.Contains(message, "auth_status") {
					t.Fatal("authorization guidance lacks recovery step")
				}
			}
			if state == clientstate.DevicePending && a.report(false, "", "").UserCode != "FIXTURE-CODE" {
				t.Fatal("structured approval details missing")
			}
		})
	}
}

func TestLegacyRejectedBootstrapExplainsReplacementWithoutRawHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture-raw-secret", http.StatusUnauthorized)
	}))
	defer server.Close()
	message := configuredRejection(t, server.URL, "fixture-legacy-token")
	if !strings.Contains(message, "replace") || !strings.Contains(message, "attach_session") || strings.Contains(message, "fixture") || strings.Contains(message, server.URL) {
		t.Fatal("bootstrap guidance unsafe or incomplete")
	}
}
