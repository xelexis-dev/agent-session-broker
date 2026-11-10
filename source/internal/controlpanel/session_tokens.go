package controlpanel

import "slices"

// RevokeSessionTokens releases reservations belonging to this exact detached
// session. The trusted runtime calls this after authorizing the UI operation,
// or when cleaning up an already detached registration. Matching the binding
// as well as the name protects a newly issued, unbound replacement token.
func (s *Store) RevokeSessionTokens(project, nickname, identity string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Projects[project]
	if s.closed || !ok || p.Deleting || identity == "" {
		return ErrStorage
	}
	tokens := append([]SessionToken(nil), p.Tokens...)
	changed := false
	revoked := map[string]bool{}
	for i, token := range tokens {
		if tokenReservesName(token, nickname) && token.BoundIdentity == identity {
			tokens[i].Revoked = true
			if !token.Revoked {
				revoked[token.ID] = true
			}
			changed = true
		}
	}
	var events []AuditEvent
	p.AutoBindings = slices.Clone(p.AutoBindings)
	for i, b := range p.AutoBindings {
		if b.Identity == identity {
			p.AutoBindings[i].DetachPending = true
			if revoked[b.GrantID] {
				events = append(events, auditFor(project, b, b.OwnerID, "revoked", "explicit-release", s.now()))
				delete(revoked, b.GrantID)
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	p.Tokens = tokens
	if err := s.commitDirectProject(p, events); err != nil {
		return err
	}
	s.notifyTokenChangesLocked(project, p.Tokens)
	s.changed(project)
	return nil
}

// Panel revocation is audited before record removal, even when removal fails.
func pendingDetachRevoked(p Project, b AutoBinding) bool {
	if b.DetachPending {
		for _, token := range p.Tokens {
			if token.ID == b.GrantID {
				return token.Revoked
			}
		}
	}
	return false
}
