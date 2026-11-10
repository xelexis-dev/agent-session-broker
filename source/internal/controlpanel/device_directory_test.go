package controlpanel

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestDeviceDirectoryNeverPersistsAndStrictlyReloads(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	owner := loginPanelUser(t, f.h, "owner")
	token := issuedTokenFrom(t, owner.call("GET", issueProjectResult(t, owner, "directory hint"), nil))
	form := deviceForm("11111111-1111-4111-8111-111111111111", "directory-req")
	form.Set("cwd_basename", "only-memory")
	_, issued := f.rest("POST", "/oauth/device_authorization?project=demo", token, form)
	raw, err := readStoredControlState(f.store.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("cwdBasename")) || bytes.Contains(raw, []byte("only-memory")) {
		t.Error("directory hint persisted in control JSON")
	}
	var persisted state
	if err = decodeControlState(raw, &persisted); err != nil {
		t.Error("strict control decoder rejected state")
	}
	if err = decodeSQLiteControlState(raw, &persisted); err != nil {
		t.Error("strict SQLite decoder rejected state")
	}
	// Independently use the released request's exact field allowlist.
	var envelope struct {
		Projects map[string]struct {
			Devices []struct {
				Request json.RawMessage `json:"request"`
			} `json:"devices"`
		} `json:"projects"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		t.Fatal("control JSON invalid")
	}
	for _, device := range envelope.Projects["demo"].Devices {
		var old struct {
			ClientID   string `json:"clientID"`
			Harness    string `json:"harness"`
			SessionID  string `json:"sessionID"`
			InstanceID string `json:"instanceID"`
			RequestID  string `json:"requestID"`
		}
		decoder := json.NewDecoder(bytes.NewReader(device.Request))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&old) != nil {
			t.Error("released strict DeviceRequest decoder rejects stored record")
		}
	}
	before, err := f.store.DeviceAuthorizationFor("owner", issued["user_code"].(string))
	if err != nil || before.CWDBasename != "only-memory" {
		t.Error("live directory hint missing")
	}
	root := f.store.root.Name()
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(root)
	if err != nil {
		t.Fatal("restart could not load compatible state")
	}
	defer reopened.Close()
	after, err := reopened.DeviceAuthorizationFor("owner", issued["user_code"].(string))
	if err != nil || (after.CWDBasename != "" && after.CWDBasename != ".") {
		t.Error("directory hint survived restart")
	}
}

func TestDeviceDirectoryHintsPruneOnReplacementCompletionAndExpiry(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	owner := loginPanelUser(t, f.h, "owner")
	token := issuedTokenFrom(t, owner.call("GET", issueProjectResult(t, owner, "prune hints"), nil))
	issue := func(request string) map[string]any {
		form := deviceForm("11111111-1111-4111-8111-111111111111", request)
		form.Set("cwd_basename", "memory-only")
		w, issued := f.rest("POST", "/oauth/device_authorization?project=demo", token, form)
		if w.Code != 200 {
			t.Fatal("device request failed")
		}
		return issued
	}
	_ = issue("same-request")
	second := issue("same-request")
	if len(f.store.deviceDirectoryHints) != 1 {
		t.Error("replaced code retained directory hint")
	}
	if _, e := f.store.ApproveDeviceAuthorization("owner", second["user_code"].(string), "prune-worker", false); e != nil {
		t.Fatal(e)
	}
	if len(f.store.deviceDirectoryHints) != 0 {
		t.Error("approval retained directory hint")
	}
	denied := issue("deny-request")
	if _, e := f.store.ApproveDeviceAuthorization("owner", denied["user_code"].(string), "", true); e != nil {
		t.Fatal(e)
	}
	if len(f.store.deviceDirectoryHints) != 0 {
		t.Error("denial retained directory hint")
	}
	_ = issue("expire-request")
	future := time.Now().Add(11 * time.Minute)
	f.store.mu.Lock()
	f.store.now = func() time.Time { return future }
	f.store.mu.Unlock()
	if e := f.store.SweepExpired(); e != nil {
		t.Fatal(e)
	}
	if len(f.store.deviceDirectoryHints) != 0 {
		t.Error("sweep retained expired hint without a new device request")
	}
}
