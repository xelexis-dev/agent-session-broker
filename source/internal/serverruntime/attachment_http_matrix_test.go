package serverruntime

import (
	"bytes"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAttachmentHTTPGrantMatrix(t *testing.T) {
	var foreignParent string
	r, parent := newDeviceRuntime(t, func(s *controlpanel.Store) {
		if s.SetAttachmentPolicy("admin", true) != nil || s.CreateProject("foreign") != nil {
			t.Fatal("matrix policy fixture")
		}
		var err error
		foreignParent, err = s.IssueProjectTokenFor("admin", "foreign", "synthetic matrix")
		if err != nil {
			t.Fatal("foreign project fixture")
		}
	})
	a, b, c, x := protectedRequest(91, "ignored"), protectedRequest(92, "ignored"), protectedRequest(93, "ignored"), protectedRequest(94, "ignored")
	for _, req := range []*broker.RegisterRequest{&a, &b, &c, &x} {
		req.Capabilities = []string{protocol.MessageAckCapability, protocol.AttachmentCapability}
	}
	ta := r.authorize(parent, a, "matrix-sender")
	tb := r.authorize(parent, b, "matrix-recipient")
	tc := r.authorize(parent, c, "matrix-peer")
	for _, row := range []struct {
		token string
		req   broker.RegisterRequest
	}{{ta, a}, {tb, b}, {tc, c}} {
		v := managedTool[broker.RegisterResult](t, r.bound(row.token), "register_session", row.req)
		if v.Code != "" {
			t.Fatal("matrix registration")
		}
	}
	r.project = "foreign"
	tx := r.authorize(foreignParent, x, "foreign-peer")
	r.project = "test"
	foreignHandler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		q.URL.RawQuery = "project=foreign"
		r.bound(tx).ServeHTTP(w, q)
	})
	foreignRegistration := managedTool[broker.RegisterResult](t, foreignHandler, "register_session", x)
	if foreignRegistration.Code != "" {
		t.Fatal("valid foreign credential not registered")
	}
	payload := []byte("matrix-distinctive-private-payload")
	put := attachmentRequest(t, r, "POST", "/attachments?project=test&from=matrix-sender", ta, payload)
	if put.Code != 201 {
		t.Fatal("matrix upload")
	}
	var ref protocol.Attachment
	if json.Unmarshal(put.Body.Bytes(), &ref) != nil {
		t.Fatal("matrix metadata")
	}
	var sent struct {
		IsError           bool
		StructuredContent broker.SendResult
	}
	managedRPC(t, r.bound(ta), "tools/call", map[string]any{"name": "send_message", "arguments": map[string]string{"from": "matrix-sender", "to": "matrix-recipient", "body": "matrix"}, "_meta": map[string]any{protocol.AttachmentMetaKey: ref.ID}}, &sent)
	if sent.IsError || !sent.StructuredContent.Accepted {
		t.Fatal("matrix bind")
	}
	good := attachmentRequest(t, r, "GET", "/attachments/"+ref.ID+"?project=test&recipient=matrix-recipient", tb, nil)
	if good.Code != 200 || !bytes.Equal(good.Body.Bytes(), payload) {
		t.Fatal("recipient positive control")
	}
	absent := strings.Repeat("A", 26)
	if absent == ref.ID {
		absent = strings.Repeat("B", 26)
	}
	check := func(t *testing.T, token, project, recipient string, want int) {
		t.Helper()
		suffix := "?project=" + url.QueryEscape(project) + "&recipient=" + url.QueryEscape(recipient)
		found := attachmentRequest(t, r, "GET", "/attachments/"+ref.ID+suffix, token, nil)
		missing := attachmentRequest(t, r, "GET", "/attachments/"+absent+suffix, token, nil)
		if want != 0 && found.Code != want {
			t.Fatalf("refusal status=%d want=%d", found.Code, want)
		}
		if found.Code == 200 || missing.Code == 200 || bytes.Contains(found.Body.Bytes(), payload) || bytes.Contains(missing.Body.Bytes(), payload) || found.Header().Get("Content-Type") == "application/octet-stream" {
			t.Fatal("unauthorized attachment bytes streamed")
		}
		if found.Code != missing.Code || !bytes.Equal(found.Body.Bytes(), missing.Body.Bytes()) {
			t.Fatal("attachment existence exposed by refusal status/body")
		}
	}
	// Stop the matrix immediately on any authorization/existence failure. The
	// remaining cases must not hide a preceding authorization failure.
	if !t.Run("same-project-spoof-recipient", func(t *testing.T) { check(t, tc, "test", "matrix-recipient", 403) }) {
		return
	}
	if !t.Run("same-project-current-session-query", func(t *testing.T) { check(t, tc, "test", "matrix-peer", 0) }) {
		return
	}
	if !t.Run("foreign-project-credential", func(t *testing.T) { check(t, tx, "test", "matrix-recipient", 401) }) {
		return
	}
	if !t.Run("bootstrap-project-token", func(t *testing.T) { check(t, parent, "test", "matrix-recipient", 401) }) {
		return
	}
	if !t.Run("sender-cannot-download", func(t *testing.T) { check(t, ta, "test", "matrix-sender", 0) }) {
		return
	}
	if !t.Run("upload-other-session-and-project", func(t *testing.T) {
		w := attachmentRequest(t, r, "POST", "/attachments?project=test&from=matrix-recipient", ta, payload)
		if w.Code != 403 || bytes.Contains(w.Body.Bytes(), payload) {
			t.Fatal("uploaded as other session")
		}
		w = attachmentRequest(t, r, "POST", "/attachments?project=test&from=matrix-sender", tx, payload)
		if w.Code != 401 {
			t.Fatal("foreign project upload accepted")
		}
		var result struct {
			IsError           bool
			StructuredContent broker.SendResult
		}
		managedRPC(t, r.bound(tc), "tools/call", map[string]any{"name": "send_message", "arguments": map[string]string{"from": "matrix-sender", "to": "matrix-recipient", "body": "spoof"}, "_meta": map[string]any{protocol.AttachmentMetaKey: ref.ID}}, &result)
		if !result.IsError && result.StructuredContent.Accepted {
			t.Fatal("bound as other sender")
		}
	}) {
		return
	}
	for _, state := range []string{"suspend", "unsuspend", "detach"} {
		if state == "unsuspend" {
			w := r.browser("POST", "/sessions/unsuspend", url.Values{"project": {"test"}, "id": {managedTool[broker.Session](t, r.bound(ta), "get_session", map[string]string{"name": "matrix-recipient"}).RegistrationID}})
			if w.Code != 303 {
				t.Fatal("unsuspend fixture")
			}
			continue
		}
		if !t.Run("recipient-"+state, func(t *testing.T) {
			recipient := managedTool[broker.Session](t, r.bound(ta), "get_session", map[string]string{"name": "matrix-recipient"})
			w := r.browser("POST", "/sessions/"+state, url.Values{"project": {"test"}, "id": {recipient.RegistrationID}})
			if w.Code != 303 {
				t.Fatal("state fixture")
			}
			check(t, tb, "test", "matrix-recipient", 0)
		}) {
			return
		}
	}
	t.Run("recipient-revoke", func(t *testing.T) {
		d := protectedRequest(95, "ignored")
		d.Capabilities = b.Capabilities
		td, grantID := r.authorizeGrant(parent, d, "matrix-revokee")
		if grantID == "" {
			t.Fatal("missing grant ID")
		}
		registered := managedTool[broker.RegisterResult](t, r.bound(td), "register_session", d)
		if registered.Code != "" {
			t.Fatal("revoke registration")
		}
		put := attachmentRequest(t, r, "POST", "/attachments?project=test&from=matrix-sender", ta, payload)
		if put.Code != 201 || json.Unmarshal(put.Body.Bytes(), &ref) != nil {
			t.Fatal("revoke upload")
		}
		managedRPC(t, r.bound(ta), "tools/call", map[string]any{"name": "send_message", "arguments": map[string]string{"from": "matrix-sender", "to": "matrix-revokee", "body": "revoke"}, "_meta": map[string]any{protocol.AttachmentMetaKey: ref.ID}}, &sent)
		if sent.IsError || !sent.StructuredContent.Accepted {
			t.Fatal("revoke bind")
		}
		good := attachmentRequest(t, r, "GET", "/attachments/"+ref.ID+"?project=test&recipient=matrix-revokee", td, nil)
		if good.Code != 200 || !bytes.Equal(good.Body.Bytes(), payload) {
			t.Fatal("pre-revoke positive control")
		}
		w := r.browser("POST", "/tokens/revoke", url.Values{"project": {"test"}, "id": {grantID}})
		if w.Code != 303 {
			t.Fatal("revoke fixture")
		}
		check(t, td, "test", "matrix-revokee", 401)
	})

}
