package channel

import (
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

func (f *deviceModeFixture) codeRequestIDs() []string {
	f.auth.mu.Lock()
	defer f.auth.mu.Unlock()
	var ids []string
	for i, r := range f.auth.requests {
		if r.URL.Path == "/oauth/device_authorization" {
			ids = append(ids, f.auth.forms[i]["request_id"])
		}
	}
	return ids
}

// A lost code answer is retried with the same request id, recorded before the
// request leaves, so the broker replaces the pending code instead of adding
// a second one.
func TestDeviceModeLostCodeAnswerRetriesWithTheSameRequestID(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	var persisted []clientstate.SessionState
	f := newDeviceModeFixture(t, auth, upstream, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.lossyCodes = 1
		f.persist = func(s *clientstate.Store, key string, state clientstate.SessionState) error {
			f.mu.Lock()
			persisted = append(persisted, state)
			f.mu.Unlock()
			return s.SaveSession(key, state)
		}
	})
	waitFor(t, "retry after the lost answer", func() bool { return len(f.codeRequestIDs()) >= 2 })
	ids := f.codeRequestIDs()
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("retry changed the request id: %v", ids)
	}
	waitFor(t, "pending code", func() bool { return f.status("")["status"] == "pending" })
	f.mu.Lock()
	recordedBeforeAnswer := false
	for _, state := range persisted {
		if state.Device != nil && state.Device.Status == clientstate.DeviceRequested && state.Device.RequestID == ids[0] && state.Device.DeviceCode == "" {
			recordedBeforeAnswer = true
		}
	}
	f.mu.Unlock()
	if !recordedBeforeAnswer {
		t.Fatal("request id was not recorded before the answer arrived")
	}
}

// A restart between a lost answer and the retry still reuses the request id.
func TestDeviceModeRestartReusesTheRequestIDOfALostCodeRequest(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	stateDir := t.TempDir()
	first := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity(), func(f *deviceModeFixture) { f.lossyCodes = 1 << 20 })
	waitFor(t, "first attempt", func() bool { return len(first.codeRequestIDs()) >= 1 })
	first.stop()
	second := newDeviceModeFixture(t, auth, upstream, stateDir, singleIdentity())
	waitFor(t, "pending after restart", func() bool { return second.status("")["status"] == "pending" })
	ids := second.codeRequestIDs()
	if ids[len(ids)-1] != ids[0] {
		t.Fatalf("restart generated a new request id: %v", ids)
	}
}

// After a denial the explicit re-request must use a fresh request id: the
// broker keeps the denied record under the old one.
func TestDeviceModeExplicitRequestAfterDenialUsesANewRequestID(t *testing.T) {
	auth := newDeviceAuthServer(t)
	upstream := newSwitchableUpstream(t)
	f := newDeviceModeFixture(t, auth, upstream, t.TempDir(), singleIdentity())
	waitFor(t, "code request", func() bool { return len(f.codeRequestIDs()) == 1 })
	f.deny("11111111-1111-4111-8111-111111111111")
	waitFor(t, "denied", func() bool { return f.status("")["status"] == "denied" })
	f.call("", "attach_session", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	ids := f.codeRequestIDs()
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("explicit request after denial: %v", ids)
	}
}
