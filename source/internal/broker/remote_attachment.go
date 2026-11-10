package broker

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"io"
)

func (s *RemoteService) SetAttachments(store *attachments.Store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachments = store
	for _, e := range s.sessions {
		e.attachments = store
	}
}
func (s *RemoteService) AttachmentsAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attachments != nil
}
func (s *RemoteService) attachmentOwnerLocked(ctx context.Context, selector string) (attachments.Owner, error) {
	g, ok := access.FromContext(ctx)
	e := s.find(selector)
	if !ok || g.Project == "" || g.TokenID == "" || e == nil || e.record == nil {
		return attachments.Owner{}, ErrSessionIdentityUnauthorized
	}
	if err := s.authorizeLocked(ctx, e); err != nil {
		return attachments.Owner{}, err
	}
	if suspendedSession(e) {
		return attachments.Owner{}, ErrSessionSuspended
	}
	if e.retired {
		return attachments.Owner{}, ErrSessionNotFound
	}
	return attachments.Owner{Project: g.Project, Registration: e.record.RegistrationID, Token: g.TokenID}, nil
}
func (s *RemoteService) deleteRejectedAttachment(ctx context.Context, from, id string, bindingSequence uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attachments == nil {
		return
	}
	if owner, err := s.attachmentOwnerLocked(ctx, from); err == nil {
		s.attachments.DeleteOwned(owner, id, bindingSequence)
	}
}
func (s *RemoteService) UploadAttachment(ctx context.Context, from, name string, size int64, body io.Reader) (protocol.Attachment, error) {
	if err := access.RequireScope(ctx, access.MessageSend); err != nil {
		return protocol.Attachment{}, err
	}
	s.mu.Lock()
	owner, err := s.attachmentOwnerLocked(ctx, from)
	store := s.attachments
	s.mu.Unlock()
	if err != nil {
		return protocol.Attachment{}, err
	}
	if store == nil {
		return protocol.Attachment{}, &attachments.Error{Status: 403, Code: "attachments-disabled"}
	}
	return store.Upload(ctx, owner, name, size, body)
}
func (s *RemoteService) DownloadAttachment(ctx context.Context, recipient, id string) (*attachments.Download, error) {
	if err := access.RequireScope(ctx, access.MessageReceive); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, err := s.attachmentOwnerLocked(ctx, recipient)
	if err != nil {
		return nil, err
	}
	if s.attachments == nil {
		return nil, &attachments.Error{Status: 403, Code: "attachments-disabled"}
	}
	return s.attachments.Get(owner.Project, owner.Registration, id)
}
func projectAttachment(m Message, supported bool, available bool) Message {
	if m.Attachment != nil && (!supported || !available) {
		m.Attachment = nil
		code := "recipient-unsupported"
		if supported {
			code = "unavailable"
		}
		m.Body += "\n[attachment unavailable: " + code + "]"
	}
	return m
}
