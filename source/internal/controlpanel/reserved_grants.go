package controlpanel

import "encoding/json"

// An approved grant reserves its nickname even before the client registers.
// Match both identity and the reserved name: missing and suspended registrations
// still belong to the existing registration row.
func grantHasRegistration(token SessionToken, sessions []Session) bool {
	for _, session := range sessions {
		if token.BoundIdentity != "" && token.BoundIdentity == sessionIdentity(session) && tokenReservesName(token, session.Name) {
			return true
		}
		if token.BoundIdentity == "" && tokenReservesName(token, session.Name) {
			return true
		}
	}
	return false
}

func (s *Store) reservedSessionGrants(project string, sessions []Session) ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStorage
	}
	if err := s.expireLocked(); err != nil {
		return nil, err
	}
	p, ok := s.data.Projects[project]
	if !ok || p.Deleting {
		return nil, ErrInvalid
	}
	return reservedSessionRows(p, s.data.Users, sessions), nil
}

func reservedSessionRows(p Project, users map[string]userRecord, sessions []Session) []Session {
	var rows []Session
	for _, token := range p.Tokens {
		if token.Nickname == "" || !tokenReservesName(token, token.Nickname) || grantHasRegistration(token, sessions) {
			continue
		}
		row := Session{ID: "grant:" + token.ID, GrantID: token.ID, Name: token.Nickname, Status: "unregistered", PendingNickname: token.PendingNickname}
		if owner, ok := users[token.OwnerID]; ok {
			row.GrantOwner = owner.Username
		}
		var identity [3]string
		if json.Unmarshal([]byte(token.BoundIdentity), &identity) == nil {
			row.Harness, row.SessionID, row.ClientID = identity[0], identity[1], identity[2]
		}
		rows = append(rows, row)
	}
	return rows
}

// Releasing an unregistered grant must never cascade to its parent or siblings.
// Authorization and revocation share the store lock; the backend snapshot must
// have succeeded before this method is called.
func (s *Store) releaseReservedGrantFor(actor, project, id string, sessions []Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStorage
	}
	if err := s.expireLocked(); err != nil {
		return err
	}
	p, ok := s.data.Projects[project]
	if !ok || p.Deleting {
		return ErrInvalid
	}
	for i, token := range p.Tokens {
		if token.ID != id {
			continue
		}
		if token.Nickname == "" || !tokenReservesName(token, token.Nickname) || grantHasRegistration(token, sessions) {
			return ErrInvalid
		}
		if !s.canManageOwnedSessionLocked(actor, project, token.OwnerID) {
			return ErrForbidden
		}
		p.Tokens = append([]SessionToken(nil), p.Tokens...)
		p.Tokens[i].Revoked = true
		p, events := s.releaseDirectGrantLocked(p, id)
		if err := s.commitDirectProject(p, events); err != nil {
			return err
		}
		s.changed(project)
		return nil
	}
	return ErrInvalid
}

// CanReleaseReservedGrant uses the same owner/role rule as the locked POST path.
func (s *Store) CanReleaseReservedGrant(actor, project, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.expireLocked() != nil {
		return false
	}
	return s.canReleaseReservedGrantLocked(actor, project, id)
}

func (s *Store) canReleaseReservedGrantLocked(actor, project, id string) bool {
	p, ok := s.data.Projects[project]
	if !ok || p.Deleting {
		return false
	}
	for _, token := range p.Tokens {
		if token.ID == id && token.Nickname != "" && tokenReservesName(token, token.Nickname) {
			return s.canManageOwnedSessionLocked(actor, project, token.OwnerID)
		}
	}
	return false
}
