package broker

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// SendAnnouncement is an internal administrator entry point, never an MCP tool.
// The control panel/runtime must authorize the administrator before calling it.
func (s *RemoteService) SendAnnouncement(ctx context.Context, recipient, body string) (SendResult, error) {
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	to := s.find(recipient)
	if to == nil {
		return SendResult{}, ErrSessionNotFound
	}
	if suspendedSession(to) {
		return SendResult{}, ErrSessionSuspended
	}
	s.expire(to)
	if to.retired || !to.session.AutomaticDeliveryReady {
		return SendResult{}, errors.New("recipient must have activated its delivery channel")
	}
	m := Message{From: protocol.AdminSender, To: to.session.Name, Body: body}
	if to.record != nil {
		m.RegistrationID = to.record.RegistrationID
	}
	if err := protocol.ValidateAnnouncement(body, m.To, m.RegistrationID); err != nil {
		return SendResult{}, err
	}
	return s.enqueueLocked(to, m)
}
