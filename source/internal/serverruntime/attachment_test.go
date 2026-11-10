package serverruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"io"
	"io/fs"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func attachmentRequest(t *testing.T, r *deviceRuntime, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "https://broker"+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-XBroker-Attachment-Name", "fixture.bin")
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}
func TestAttachmentRuntimeSecurityLifecycle(t *testing.T) {
	r, parent := newDeviceRuntime(t, func(s *controlpanel.Store) {
		if err := s.SetAttachmentPolicy("admin", true); err != nil {
			t.Fatal("enable fixture")
		}
	})
	a, b := protectedRequest(81, "ignored"), protectedRequest(82, "ignored")
	a.Capabilities = []string{protocol.MessageAckCapability, protocol.AttachmentCapability}
	b.Capabilities = a.Capabilities
	ta := r.authorize(parent, a, "file-sender")
	tb := r.authorize(parent, b, "file-receiver")
	first := managedTool[broker.RegisterResult](t, r.bound(ta), "register_session", a)
	second := managedTool[broker.RegisterResult](t, r.bound(tb), "register_session", b)
	if first.Code != "" || second.Code != "" {
		t.Fatal("register")
	}
	data := []byte{0x91, 0, 0xff, 'D', 'I', 'S', 'T', 'I', 'N', 'C', 'T', 0}
	put := attachmentRequest(t, r, "POST", "/attachments?project=test&from=file-sender", ta, data)
	if put.Code != 201 {
		t.Fatalf("upload status%d", put.Code)
	}
	var ref protocol.Attachment
	if json.Unmarshal(put.Body.Bytes(), &ref) != nil || ref.ID == "" {
		t.Fatal("upload metadata")
	}
	// A valid session credential does not grant another registration's upload access.
	stolen := attachmentRequest(t, r, "POST", "/attachments?project=test&from=file-sender", tb, data)
	if stolen.Code != 403 {
		t.Fatal("other credential uploaded as sender")
	}
	var result struct {
		IsError           bool
		StructuredContent broker.SendResult
	}
	managedRPC(t, r.bound(ta), "tools/call", map[string]any{"name": "send_message", "arguments": map[string]string{"from": "file-sender", "to": "file-receiver", "body": "file is attached"}, "_meta": map[string]any{protocol.AttachmentMetaKey: ref.ID}}, &result)
	if result.IsError || !result.StructuredContent.Accepted {
		t.Fatal("metadata bind/send")
	}
	path := "/attachments/" + ref.ID + "?project=test&recipient=file-receiver"
	wrong := attachmentRequest(t, r, "GET", path, ta, nil)
	if wrong.Code != 403 {
		t.Fatal("other credential downloaded")
	}
	got := attachmentRequest(t, r, "GET", path, tb, nil)
	if got.Code != 200 || !bytes.Equal(got.Body.Bytes(), data) || got.Header().Get("X-XBroker-Attachment-SHA256") != ref.SHA256 || fmt.Sprintf("%x", sha256.Sum256(data)) != ref.SHA256 {
		t.Fatal("download integrity")
	}
	message := managedTool[broker.Message](t, r.bound(tb), "wait_for_message", protocol.IngressRequest{Recipient: "file-receiver", ChannelID: b.ChannelID, RegistrationID: second.Session.RegistrationID, Epoch: second.Session.Epoch})
	encoded, _ := json.Marshal(message)
	if bytes.Contains(encoded, data) || bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(data))) || bytes.Contains(encoded, []byte("DISTINCT")) || message.Attachment == nil || message.Attachment.Size != int64(len(data)) {
		t.Fatal("binary bytes entered message or ref missing")
	}
	_ = managedTool[broker.RegisterResult](t, r.bound(tb), "renew_lease", broker.RenewRequest{RegistrationID: second.Session.RegistrationID, ChannelID: b.ChannelID, Epoch: second.Session.Epoch, AckThrough: message.DeliverySequence})
	if _, e := os.Stat(filepath.Join(r.dir, "attachments-v1", ref.ID)); !os.IsNotExist(e) {
		t.Fatal("ACK retained spool bytes")
	}
	put = attachmentRequest(t, r, "POST", "/attachments?project=test&from=file-sender", ta, data)
	if put.Code != 201 {
		t.Fatal("second upload")
	}
	_ = json.Unmarshal(put.Body.Bytes(), &ref)
	page := r.browser("GET", "/admin/settings", nil)
	match := regexp.MustCompile(`(?s)action="/admin/attachments".*?name="settings_version" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatal("setting version")
	}
	off := r.browser("POST", "/admin/attachments", url.Values{"settings_version": {match[1]}})
	if off.Code != 303 {
		t.Fatal("disable")
	}
	blocked := attachmentRequest(t, r, "GET", "/attachments/"+ref.ID+"?project=test&recipient=file-receiver", tb, nil)
	if blocked.Code != 403 {
		t.Fatal("disabled download allowed")
	}
	if _, e := os.Stat(filepath.Join(r.dir, "attachments-v1", ref.ID)); !os.IsNotExist(e) {
		t.Fatal("disable retained bytes")
	}
	// The persisted switch and extra spool directory do not alter strict control state.
	r.restart()
	if attachmentRequest(t, r, "POST", "/attachments?project=test&from=file-sender", ta, data).Code != 403 {
		t.Fatal("disabled switch did not survive restart")
	}
}

func TestAttachmentRuntimeCapabilitiesAndEarlyRejection(t *testing.T) {
	r, parent := newDeviceRuntime(t)
	request := protectedRequest(83, "ignored")
	token := r.authorize(parent, request, "file-peer")
	_ = managedTool[broker.RegisterResult](t, r.bound(token), "register_session", request)
	if w := attachmentRequest(t, r, "POST", "/attachments?project=test&from=file-peer", token, []byte("x")); w.Code != 403 {
		t.Fatal("default off")
	}
	if w := attachmentRequest(t, r, "GET", "/attachments/AAAAAAAAAAAAAAAAAAAAAAAAAA?project=test&recipient=file-peer", "invalid", nil); w.Code != 401 {
		t.Fatal("invalid credential")
	}
	req := httptest.NewRequest("POST", "http://broker/attachments?project=test&from=file-peer", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("HTTP policy bypass")
	}
	var init struct {
		Capabilities struct{ Experimental map[string]any }
	}
	managedRPC(t, r.bound(token), "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "fixture", "version": "0.17.3"}}, &init)
	if init.Capabilities.Experimental[protocol.AttachmentCapability] != true {
		t.Fatal("managed attachment capability missing")
	}
}

func TestAttachmentRollbackFixture(t *testing.T) {
	target := os.Getenv("ASB_ATTACHMENT_ROLLBACK_FIXTURE")
	if target == "" {
		t.Skip("opt-in synthetic rollback data fixture")
	}
	if !strings.HasPrefix(target, "/private/tmp/att1/") {
		t.Fatal("rollback fixture must be task-owned")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("rollback target already exists")
	}
	source := t.TempDir()
	policy, e := controlpanel.OpenSQLite(filepath.Join(source, "control"))
	if e != nil {
		t.Fatal("control fixture")
	}
	if policy.Setup("test-only-attachment-rollback-password") != nil || policy.CreateProject("test") != nil || policy.SetAttachmentPolicy("admin", true) != nil {
		t.Fatal("policy fixture")
	}
	spool, e := attachments.Open(source)
	if e != nil {
		t.Fatal("spool fixture")
	}
	defer spool.Close()
	spool.SetEnabled(true)
	if _, e = spool.Upload(t.Context(), attachments.Owner{Project: "test", Registration: "synthetic-registration", Token: "synthetic-token-id"}, "fixture.bin", 4, strings.NewReader("blob")); e != nil {
		t.Fatal("spool write")
	}
	if policy.Close() != nil {
		t.Fatal("control close")
	}
	err := filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, e := filepath.Rel(source, path)
		if e != nil {
			return e
		}
		dst := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0700)
		}
		if !d.Type().IsRegular() {
			return errors.New("unsafe synthetic fixture")
		}
		in, e := os.Open(path)
		if e != nil {
			return e
		}
		defer in.Close()
		out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, in)
		return errors.Join(e, out.Close())
	})
	if err != nil {
		t.Fatal("copy synthetic fixture")
	}
}
