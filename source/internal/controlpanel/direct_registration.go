package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"slices"
)

// Broker -> policy is the only nested lock order; never query the broker here.
func (s *Store) directRegistration(project, grant, identity, id, owner, name string, revision uint64, confirmed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStorage
	}
	p, ok := s.data.Projects[project]
	if !ok || !p.Enabled || p.Deleting {
		return ErrForbidden
	}
	tokenIndex := -1
	for i, t := range p.Tokens {
		if t.ID == grant && t.BoundIdentity == identity && s.sessionTokenEligibleLocked(p, t) {
			tokenIndex = i
			break
		}
	}
	if tokenIndex < 0 {
		return ErrForbidden
	}
	token := p.Tokens[tokenIndex]
	if token.Nickname != name || id == "" {
		return ErrForbidden
	}
	for i, b := range p.AutoBindings {
		if b.GrantID != grant || b.Identity != identity {
			continue
		}
		if current := latestDirectOwner(s, p, identity, grant); current != "" && current != token.OwnerID {
			return ErrForbidden
		}
		if confirmed {
			if (b.State != "claiming" && b.State != "confirmed") || b.RegistrationID != id || owner != token.OwnerID || revision == 0 || revision <= b.ExpectedRevision {
				return ErrForbidden
			}
			if b.State == "confirmed" {
				return nil
			}
			p.AutoBindings = slices.Clone(p.AutoBindings)
			p.AutoBindings[i].State = "confirmed"
			p.Tokens = slices.Clone(p.Tokens)
			p.Tokens[tokenIndex].FirstConnectedAt = s.now().UTC()
			for j, t := range p.Tokens {
				if t.ID == token.ParentID {
					p.Tokens[j].LastRegisteredAt = s.now().UTC()
				}
			}
			events := []AuditEvent{auditFor(project, b, token.OwnerID, "registration", "confirmed", s.now())}
			if b.HandoverSource != "" {
				p.AutoBindings = slices.DeleteFunc(p.AutoBindings, func(old AutoBinding) bool { return old.Identity == b.HandoverSource && old.OwnerID == token.OwnerID })
				for j, old := range p.Tokens {
					if old.BoundIdentity == b.HandoverSource && old.OwnerID == token.OwnerID {
						p.Tokens[j].Revoked = true
					}
				}
				at := s.now()
				events = append(events, AuditEvent{Version: 1, Project: project, OwnerID: token.OwnerID, Kind: "handover", At: at, Handover: &protocol.HandoverAudit{Previous: digest(b.HandoverSource), Current: digest(identity), RegistrationID: id, Name: name, At: at}})
			}
			if err := s.commitDirectProject(p, events); err != nil {
				return err
			}
			s.notifyTokenChangesLocked(project, p.Tokens)
			return nil
		}
		if b.State == "confirmed" {
			if owner == "" && revision == 0 && !b.DetachPending {
				p.AutoBindings = slices.Clone(p.AutoBindings)
				p.AutoBindings[i].State = "claiming"
				p.AutoBindings[i].RegistrationID = id
				p.AutoBindings[i].ExpectedID = ""
				p.AutoBindings[i].ExpectedOwner = ""
				p.AutoBindings[i].ExpectedRevision = 0
				return s.commitDirectProject(p, []AuditEvent{auditFor(project, b, token.OwnerID, "request", "accepted", s.now())})
			}
			if b.RegistrationID != id || owner != token.OwnerID {
				return ErrForbidden
			}
			return nil
		}
		if b.ExpectedID == "" {
			if revision != 0 || owner != "" {
				return ErrForbidden
			}
		} else if id != b.ExpectedID || revision != b.ExpectedRevision || owner != b.ExpectedOwner {
			return ErrForbidden
		}
		p.AutoBindings = slices.Clone(p.AutoBindings)
		p.AutoBindings[i].State = "claiming"
		p.AutoBindings[i].RegistrationID = id
		return s.commitDirectProject(p, []AuditEvent{auditFor(project, b, token.OwnerID, "request", "accepted", s.now())})
	}
	return ErrForbidden
}
func (s *Store) ReleaseDirectRegistration(project, identity string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStorage
	}
	p, ok := s.data.Projects[project]
	if !ok {
		return ErrForbidden
	}
	var events []AuditEvent
	removed := false
	p.AutoBindings = slices.DeleteFunc(slices.Clone(p.AutoBindings), func(b AutoBinding) bool {
		if b.Identity != identity {
			return false
		}
		removed = true
		if !pendingDetachRevoked(p, b) {
			events = append(events, auditFor(project, b, b.OwnerID, "revoked", "explicit-release", s.now()))
		}
		return true
	})
	if !removed {
		return nil
	}
	return s.commitDirectProject(p, events)
}
func (s *Store) releaseDirectGrantLocked(p Project, id string) (Project, []AuditEvent) {
	var events []AuditEvent
	p.AutoBindings = slices.DeleteFunc(slices.Clone(p.AutoBindings), func(b AutoBinding) bool {
		if b.GrantID != id {
			return false
		}
		if !pendingDetachRevoked(s.data.Projects[p.Name], b) {
			events = append(events, auditFor(p.Name, b, b.OwnerID, "revoked", "explicit-release", s.now()))
		}
		return true
	})
	return p, events
}

// Reconcile only the explicit persisted rename journal against a verified
// broker snapshot. Unrelated revision movement never silently refreshes proof.
func recoverDirectRename(p Project, b AutoBinding, proof DirectProof) (Project, AutoBinding, error) {
	if b.ExpectedID == "" {
		return p, b, nil
	}
	if proof.RegistrationID != b.ExpectedID {
		return p, b, errDirectConflict
	}
	p.Tokens = slices.Clone(p.Tokens)
	for i, t := range p.Tokens {
		if t.ID == b.GrantID {
			if b.PendingRevision != 0 && t.PendingNickname != "" {
				if proof.Name == t.PendingNickname && proof.Revision == b.PendingRevision {
					p.Tokens[i].Nickname = proof.Name
					b.ExpectedRevision = proof.Revision
				} else if proof.Name != t.Nickname || proof.Revision != b.ExpectedRevision {
					return p, b, errDirectConflict
				}
				p.Tokens[i].PendingNickname = ""
				b.PendingRevision = 0
			}
			if proof.Revision != b.ExpectedRevision || proof.OwnerID != b.ExpectedOwner {
				return p, b, errDirectConflict
			}
		}
	}
	p.AutoBindings = slices.Clone(p.AutoBindings)
	for i, v := range p.AutoBindings {
		if v.GrantID == b.GrantID {
			p.AutoBindings[i] = b
		}
	}
	return p, b, nil
}

// Only a definitive pre-apply broker refusal reaches this cleanup. Unknown
// storage/transport outcomes retain the same pending request for replay.
func (s *Store) abortConversationHandover(project, grant string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Projects[project]
	if !ok {
		return ErrForbidden
	}
	var rejected *AutoBinding
	for _, b := range p.AutoBindings {
		if b.GrantID == grant && b.HandoverSource != "" && b.State != "confirmed" {
			copy := b
			rejected = &copy
			break
		}
	}
	if rejected == nil {
		return nil
	}
	p.AutoBindings = slices.DeleteFunc(slices.Clone(p.AutoBindings), func(b AutoBinding) bool { return b.GrantID == grant })
	p.AutoExchanges = slices.DeleteFunc(slices.Clone(p.AutoExchanges), func(e AutoExchange) bool { return e.GrantID == grant })
	p.Tokens = slices.Clone(p.Tokens)
	for i, t := range p.Tokens {
		if t.ID == grant {
			p.Tokens[i].Revoked = true
		}
	}
	if err := s.commitDirectProject(p, []AuditEvent{auditFor(project, *rejected, rejected.OwnerID, "revoked", "explicit-release", s.now())}); err != nil {
		return err
	}
	s.notifyTokenChangesLocked(project, p.Tokens)
	return nil
}
