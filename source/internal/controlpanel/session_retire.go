package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"slices"
)

func (s *Store) retireCredentials(project, id string, target access.RetirementTarget, generation <-chan struct{}, detach func() error) ([]protocol.RevokedCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reject := func(code string) ([]protocol.RevokedCredential, error) { return nil, protocol.RejectRetire(code) }
	if s.closed {
		return reject("storage")
	}
	select {
	case <-generation:
		return reject("forbidden")
	default:
	}
	p, ok := s.data.Projects[project]
	if !ok || !p.Enabled || p.Deleting {
		return reject("forbidden")
	}
	var actor SessionToken
	for _, t := range p.Tokens {
		if t.ID == id && s.sessionTokenEligibleLocked(p, t) {
			actor = t
			break
		}
	}
	if actor.ID == "" || actor.OwnerID == "" {
		return reject("forbidden")
	}
	if target.RegistrationID != "" && !s.canManageSessionLocked(actor.OwnerID, project, target.Identity, target.RecordedOwner) {
		return reject("forbidden")
	}
	var indices []int
	for i, t := range p.Tokens {
		if t.Kind == tokenProject {
			continue
		}
		if target.RegistrationID != "" {
			if t.BoundIdentity != target.Identity {
				continue
			}
			// Include already revoked grants on cleanup retry, but never unused replacements.
			if t.FirstConnectedAt.IsZero() {
				if !t.Revoked {
					return reject("credential-unused")
				}
				continue
			}
		} else {
			if !tokenReservesName(t, target.Name) {
				continue
			}
			if !s.canManageOwnedSessionLocked(actor.OwnerID, project, t.OwnerID) {
				return reject("forbidden")
			}
			if t.FirstConnectedAt.IsZero() {
				return reject("credential-unused")
			}
		}
		if t.ID == id {
			return reject("self-retire-denied")
		}
		indices = append(indices, i)
	}
	if len(indices) == 0 && target.RegistrationID == "" {
		return reject("not-found")
	}
	if detach != nil {
		if err := detach(); err != nil {
			return reject("storage")
		}
	}
	p.Tokens = slices.Clone(p.Tokens)
	revoked := make([]protocol.RevokedCredential, 0, len(indices))
	var events []AuditEvent
	for _, i := range indices {
		t := p.Tokens[i]
		p.Tokens[i].Revoked = true
		revoked = append(revoked, protocol.RevokedCredential{ID: t.ID, FirstConnectedAt: t.FirstConnectedAt})
		var released []AuditEvent
		p, released = s.releaseDirectGrantLocked(p, t.ID)
		events = append(events, released...)
		p.AutoExchanges = slices.DeleteFunc(slices.Clone(p.AutoExchanges), func(e AutoExchange) bool { return e.GrantID == t.ID })
	}
	if target.RegistrationID != "" {
		event := target.Event
		// A retry after failed record removal must not duplicate the completed audit.
		recorded := false
		for _, a := range s.audits {
			if a.Project == project && a.Management != nil && a.Management.Retired && a.Management.TargetRegistrationID == target.RegistrationID {
				recorded = true
				break
			}
		}
		if !recorded {
			events = append(events, AuditEvent{Version: 1, Project: project, OwnerID: actor.OwnerID, Kind: "detached", At: event.At, Management: &event})
		}
	}
	if err := s.commitDirectProject(p, events); err != nil {
		return reject("storage")
	}
	s.notifyTokenChangesLocked(project, p.Tokens)
	// Do not signal changed(project): it would cancel the caller and other sessions.
	return revoked, nil
}
