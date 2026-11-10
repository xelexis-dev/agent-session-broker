package broker

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func attachmentBroker(t *testing.T) (*RemoteService, *attachments.Store, string, RegisterRequest, RegisterResult, context.Context) {
	t.Helper()
	s, _, r, out := ackFixture(t)
	dir := t.TempDir()
	a, e := attachments.Open(dir)
	if e != nil {
		t.Fatal("open")
	}
	t.Cleanup(func() { a.Close() })
	a.SetEnabled(true)
	s.SetAttachments(a)
	ctx := access.WithGrant(t.Context(), access.Grant{Project: "demo", TokenID: "token", OwnerID: "owner", Nickname: r.Name})
	return s, a, dir, r, out, ctx
}
func brokerUpload(t *testing.T, a *attachments.Store, out RegisterResult) string {
	t.Helper()
	ref, e := a.Upload(t.Context(), attachments.Owner{Project: "demo", Registration: out.Session.RegistrationID, Token: "token"}, "f.bin", 4, strings.NewReader("blob"))
	if e != nil {
		t.Fatal("upload")
	}
	return ref.ID
}
func TestAttachmentBindRefusalsDelete(t *testing.T) {
	for _, reason := range []string{"capability", "ack", "recipient-changed", "queue-full", "disabled"} {
		t.Run(reason, func(t *testing.T) {
			s, a, dir, r, out, ctx := attachmentBroker(t)
			if reason != "capability" {
				resume := resumeFrom(r, out.Session.RegistrationID, "refusal-lease", "refusal-resume", out.Session.Revision)
				resume.Capabilities = []string{protocol.MessageAckCapability, protocol.AttachmentCapability}
				if reason == "ack" {
					resume.Capabilities = []string{protocol.AttachmentCapability}
				}
				next, err := s.ResumeSession(ctx, resume)
				if err != nil || next.Code != "" {
					t.Fatal("refusal setup")
				}
				out = next
			}
			id := brokerUpload(t, a, out)
			req := Message{From: r.Name, To: r.Name, Body: "hello", Attachment: &protocol.Attachment{ID: id}}
			if reason == "recipient-changed" {
				v := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
				req.ExpectRegistrationID = &v
			}
			if reason == "queue-full" {
				for i := 0; i < 32; i++ {
					if _, e := s.SendMessage(ctx, Message{From: r.Name, To: r.Name, Body: "fill"}); e != nil {
						t.Fatal("fill")
					}
				}
			}
			if reason == "disabled" {
				a.SetEnabled(false)
			}
			result, e := s.SendMessage(ctx, req)
			if result.Accepted || (e == nil && result.Code == "") {
				t.Fatal("attachment refusal accepted")
			}
			if _, e = os.Stat(filepath.Join(dir, "attachments-v1", id)); !os.IsNotExist(e) {
				t.Fatal("failed send retained upload")
			}
		})
	}
}
func TestAttachmentAckDiscardAndCopyProjection(t *testing.T) {
	for _, kind := range []string{"ack", "discard", "downgrade"} {
		t.Run(kind, func(t *testing.T) {
			s, a, dir, r, out, ctx := attachmentBroker(t)
			resume := resumeFrom(r, out.Session.RegistrationID, "attachment-lease", "attachment-resume", out.Session.Revision)
			resume.Capabilities = []string{protocol.MessageAckCapability, protocol.AttachmentCapability}
			next, e := s.ResumeSession(ctx, resume)
			if e != nil || next.Code != "" {
				t.Fatal("resume")
			}
			r.ChannelID = resume.ChannelID
			out = next
			id := brokerUpload(t, a, out)
			sent, e := s.SendMessage(ctx, Message{From: r.Name, To: r.Name, Body: "hello", Attachment: &protocol.Attachment{ID: id}})
			if e != nil || !sent.Accepted {
				t.Fatal("send")
			}
			m := ackWait(t, s, r, out, 0, 0)
			if m.Attachment == nil || m.Attachment.Size != 4 || m.Attachment.SHA256 == "" {
				t.Fatal("not server-owned attachment")
			}
			entry := s.byRegistration[out.Session.RegistrationID]
			if kind == "downgrade" {
				original, _ := json.Marshal(entry.inflight)
				before := entry.inflightBytes
				downgraded := resumeFrom(r, out.Session.RegistrationID, "old-lease", "old-resume", out.Session.Revision)
				downgraded.Capabilities = []string{protocol.MessageAckCapability}
				v, e := s.ResumeSession(ctx, downgraded)
				if e != nil || v.Code != "" {
					t.Fatal("downgrade")
				}
				r.ChannelID = downgraded.ChannelID
				out = v
				m = ackWait(t, s, r, out, 0, 0)
				after, _ := json.Marshal(entry.inflight)
				if m.Attachment != nil || !strings.HasSuffix(m.Body, "[attachment unavailable: recipient-unsupported]") || entry.inflightBytes != before || string(original) != string(after) {
					t.Fatal("projection mutated inflight or failed")
				}
				if _, e = os.Stat(filepath.Join(dir, "attachments-v1", id)); e != nil {
					t.Fatal("downgrade deleted before ACK")
				}
			}
			if kind == "discard" {
				if e := s.StopSession(ctx, r.Name); e != nil {
					t.Fatal("discard")
				}
			} else {
				_, e := s.RenewLease(ctx, RenewRequest{RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID, Epoch: out.Session.Epoch, AckThrough: m.DeliverySequence})
				if e != nil {
					t.Fatal("ack")
				}
			}
			if _, e = os.Stat(filepath.Join(dir, "attachments-v1", id)); !os.IsNotExist(e) {
				t.Fatal("ack/discard retained bytes")
			}
		})
	}
}

func TestAttachmentReserveUsesEncodedEnvelope(t *testing.T) {
	s, a, dir, r, out, ctx := attachmentBroker(t)
	resume := resumeFrom(r, out.Session.RegistrationID, "budget-lease", "budget-resume", out.Session.Revision)
	resume.Capabilities = []string{protocol.MessageAckCapability, protocol.AttachmentCapability}
	next, e := s.ResumeSession(ctx, resume)
	if e != nil || next.Code != "" {
		t.Fatal("resume")
	}
	out = next
	body := strings.Repeat("&", 1800)
	plain, e := s.SendMessage(ctx, Message{From: r.Name, To: r.Name, Body: body})
	if e != nil || !plain.Accepted {
		t.Fatal("plain legacy budget changed")
	}
	id := brokerUpload(t, a, out)
	sent, e := s.SendMessage(ctx, Message{From: r.Name, To: r.Name, Body: body, Attachment: &protocol.Attachment{ID: id}})
	if sent.Accepted || e == nil {
		t.Fatal("encoded attachment reserve ignored")
	}
	if _, e := os.Stat(filepath.Join(dir, "attachments-v1", id)); !os.IsNotExist(e) {
		t.Fatal("oversize retained upload")
	}
}

func TestAttachmentRejectedRepeatPreservesAcceptedBinding(t *testing.T) {
	s, a, dir, r, out, ctx := attachmentBroker(t)
	resume := resumeFrom(r, out.Session.RegistrationID, "repeat-lease", "repeat-resume", out.Session.Revision)
	resume.Capabilities = []string{protocol.MessageAckCapability, protocol.AttachmentCapability}
	next, err := s.ResumeSession(ctx, resume)
	if err != nil || next.Code != "" {
		t.Fatal("resume")
	}
	out = next
	r.ChannelID = resume.ChannelID
	id := brokerUpload(t, a, out)
	message := Message{From: r.Name, To: r.Name, Body: "hello", Attachment: &protocol.Attachment{ID: id}}
	sent, err := s.SendMessage(ctx, message)
	if err != nil || !sent.Accepted {
		t.Fatal("initial send")
	}
	repeated, err := s.SendMessage(ctx, message)
	if repeated.Accepted || err == nil && repeated.Code == "" {
		t.Fatal("repeat accepted")
	}
	d, err := a.Get("demo", out.Session.RegistrationID, id)
	if err != nil {
		t.Fatal("repeat deleted accepted attachment")
	}
	data, err := io.ReadAll(d)
	d.Close()
	if err != nil || string(data) != "blob" {
		t.Fatal("accepted download damaged")
	}
	m := ackWait(t, s, r, out, 0, 0)
	if _, err = s.RenewLease(ctx, RenewRequest{RegistrationID: out.Session.RegistrationID, ChannelID: r.ChannelID, Epoch: out.Session.Epoch, AckThrough: m.DeliverySequence}); err != nil {
		t.Fatal("ACK")
	}
	if _, err = os.Stat(filepath.Join(dir, "attachments-v1", id)); !os.IsNotExist(err) {
		t.Fatal("ACK retained bytes")
	}
}
