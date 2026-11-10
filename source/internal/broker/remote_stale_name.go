package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const observedNameLimit = 256

type observedPeer struct {
	registrationID string
	ambiguous      bool
	updated        uint64
}

// All observation and confirmation runs under the same lock as name routing.
func (s *RemoteService) observePeerLocked(ctx context.Context, sender *remoteSession, name, id string, confirm bool) {
	if sender.record == nil || id == "" || protocol.IsAdminSender(name) || access.RequireScope(ctx, access.SessionRead) != nil {
		return
	}
	if sender.observedPeers == nil {
		sender.observedPeers = make(map[string]observedPeer)
	}
	peer, exists := sender.observedPeers[name]
	if !exists && len(sender.observedPeers) >= observedNameLimit {
		var oldest string
		var update uint64
		for name, p := range sender.observedPeers {
			if oldest == "" || p.updated < update {
				oldest, update = name, p.updated
			}
		}
		delete(sender.observedPeers, oldest)
	}
	if confirm || !exists {
		peer.registrationID, peer.ambiguous = id, false
	} else if peer.registrationID != id {
		peer.ambiguous = true
	}
	sender.peerSequence++
	peer.updated = sender.peerSequence
	sender.observedPeers[name] = peer
}

func (s *RemoteService) staleNameLocked(ctx context.Context, from, to *remoteSession, m Message) bool {
	if from.record == nil || m.ExpectRegistrationID != nil || access.RequireScope(ctx, access.SessionRead) != nil {
		return false
	}
	name := strings.TrimPrefix(m.To, SelectorName)
	if s.sessions[name] != to {
		return false
	}
	if !strings.HasPrefix(m.To, SelectorName) && (typedSelector(m.To) || s.byRegistration[m.To] != nil) {
		return false
	}
	peer, exists := from.observedPeers[name]
	id := ""
	if to.record != nil {
		id = to.record.RegistrationID
	}
	return exists && (peer.ambiguous || peer.registrationID != id)
}

func staleNameMessage(to string) string {
	return fmt.Sprintf("%s now belongs to a different registration than the one this session last exchanged messages with. Nothing was sent. Call get_session for %s to check the current holder, then resend only if you mean that session.", to, to)
}

// GetSessionObserved confirms only a name read through a session-bound credential.
// Ordinary internal GetSession calls and unbound roster reads remain observations-free.
func (s *RemoteService) GetSessionObserved(ctx context.Context, selector string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	to := s.find(selector)
	if to == nil {
		return Session{}, ErrSessionNotFound
	}
	grant, bound := access.FromContext(ctx)
	if bound && grant.Check != nil && access.RequireScope(ctx, access.SessionRead) == nil && to.record != nil {
		name := strings.TrimPrefix(selector, SelectorName)
		if s.sessions[name] == to && (strings.HasPrefix(selector, SelectorName) || !typedSelector(selector) && s.byRegistration[selector] == nil) {
			for _, from := range s.byRegistration {
				if grant.Check(bindingForRename(from)) == nil && s.authorizeLocked(ctx, from) == nil {
					s.observePeerLocked(ctx, from, name, to.record.RegistrationID, true)
					break
				}
			}
		}
	}
	return s.snapshot(to), nil
}
