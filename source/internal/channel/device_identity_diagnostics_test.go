package channel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestDeviceIdentityInsecureSecretAuthStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	path := filepath.Join(home, "state", "x-broker", "device-secret")
	if runtime.GOOS == "darwin" {
		path = filepath.Join(home, "Library", "Application Support", "x-broker", "device-secret")
	} else if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", home)
		path = filepath.Join(home, "x-broker", "device-secret")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal("fixture directory")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("01234567", 8)+"\n"), 0644); err != nil {
		t.Fatal("fixture secret")
	}
	private := insecureSecretFixture(t, path)
	identity := clientstate.NewReceiverIdentity("fixture")
	if identity.Current(t.Context()) != nil {
		t.Fatal("insecure material accepted")
	}
	f := newDeviceModeFixture(t, newDeviceAuthServer(t), newSwitchableUpstream(t), t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
		f.configureDurable = func(o *durableOptions) { o.receiverIdentity = identity }
	})
	status := f.status("")
	if status["deviceIdentity"] != "secret-insecure" {
		t.Fatal("auth_status must explain insecure secret without exposing it")
	}
	private()
	holder := identity.Current(t.Context())
	if holder == nil {
		t.Fatal("valid fixture identity")
	}
	visible, _ := json.Marshal(f.status(""))
	for _, sentinel := range []string{home, path, strings.Repeat("01234567", 8), holder.DeviceID, holder.Incarnation} {
		if strings.Contains(string(visible), sentinel) {
			t.Fatal("device material escaped auth_status")
		}
	}
}

type diagnosticCauseIdentity struct {
	fixtureReceiverIdentity
	cause string
}

func (i *diagnosticCauseIdentity) DiagnosticCause() string { return i.cause }

func TestDeviceIdentitySafeHintsAndCauseChangeEvents(t *testing.T) {
	s, _ := clientstate.Open(t.TempDir())
	id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111"}
	key := clientstate.RegistrationKey("fixture", id.Harness, id.SessionID)
	identity := &diagnosticCauseIdentity{cause: "path-unavailable"}
	opts := durableOptions{state: s, receiverIdentity: identity}
	b := &ingressBinding{mu: &sync.Mutex{}, identity: id, durable: &durableRegistration{key: key}}
	for n := 0; n < 10; n++ {
		opts.diagnoseDeviceIdentity(b)
	}
	identity.cause = "ok"
	for n := 0; n < 10; n++ {
		opts.diagnoseDeviceIdentity(b)
	}
	reopened, _ := clientstate.Open(s.Dir())
	opts.state = reopened
	opts.diagnoseDeviceIdentity(b)
	d, err := s.LoadDiagnostics(key)
	if err != nil || len(d.Events) != 2 || d.Events[0].Reason != "path-unavailable" || d.Events[1].Reason != "ok" {
		t.Fatal("repeated cause was recorded or transition lost", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "diagnostics", key+".json")); !os.IsNotExist(err) {
		t.Fatal("device event entered frozen v1")
	}
	for n := 0; n < 260; n++ {
		if err := s.RecordDiagnostic(key, clientstate.DiagnosticEvent{At: d.Events[1].At.Add(time.Duration(n+1) * time.Second), SessionID: id.SessionID, Event: "reconnected"}); err != nil {
			t.Fatal(err)
		}
	}
	opts.diagnoseDeviceIdentity(b)
	d, err = s.LoadDiagnostics(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range d.Events {
		if e.Event == "device-identity" {
			t.Fatal("retention caused an unchanged cause to repeat")
		}
	}
	for _, code := range []string{"ok", "path-unavailable", "directory-insecure", "directory-unwritable", "lock-failed", "secret-insecure", "secret-malformed", "secret-missing", "secret-changed", "secret-create-failed", "process-identity-unavailable", "secret-created", "secret-created-lock-present"} {
		identity.cause = code
		out := opts.deviceIdentityStatus(authStatus{})
		if out.DeviceIdentity != code || out.DeviceIdentityHint == "" || strings.ContainsAny(out.DeviceIdentityHint, "\r\n") {
			t.Fatal("cause hint missing", code)
		}
		if code == "secret-created-lock-present" && out.DeviceIdentityHint != "잠금 파일이 남아 있는 상태에서 새로 만들었습니다. 이전 비밀값 이력은 알 수 없습니다" {
			t.Fatal("lock-present wording must not infer prior secret deletion")
		}
	}
	identity.cause = "private-path-sentinel/" + strings.Repeat("a", 64)
	out := opts.deviceIdentityStatus(authStatus{})
	raw, _ := json.Marshal(out)
	if out.DeviceIdentity != "unknown" || strings.Contains(string(raw), "private-") || strings.Contains(string(raw), strings.Repeat("a", 64)) {
		t.Fatal("unknown device cause escaped output")
	}
	bugRaw, _ := json.Marshal(opts.bugDiagnostics(t.Context(), b, unavailableUpstream{}))
	if strings.Contains(string(bugRaw), "private-") || strings.Contains(string(bugRaw), strings.Repeat("a", 64)) {
		t.Fatal("device sentinel escaped bug diagnostics")
	}
}
