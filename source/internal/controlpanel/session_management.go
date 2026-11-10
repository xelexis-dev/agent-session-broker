package controlpanel

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func (s *Store) checkSessionManagement(project, id, identity, owner string, generation <-chan struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.expireLocked() != nil {
		return ErrForbidden
	}
	select {
	case <-generation:
		return ErrForbidden
	default:
	}
	p, ok := s.data.Projects[project]
	if !ok || !p.Enabled || p.Deleting {
		return ErrForbidden
	}
	for _, token := range p.Tokens {
		if token.ID == id && token.OwnerID != "" && s.sessionTokenEligibleLocked(p, token) && s.canManageSessionLocked(token.OwnerID, project, identity, owner) {
			return nil
		}
	}
	return ErrForbidden
}
func (s *Store) RecordSessionManagement(project, actor string, event protocol.ManagementEvent, expectedGeneration ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStorage
	}
	p, ok := s.data.Projects[project]
	if !ok || (len(expectedGeneration) > 0 && (len(expectedGeneration) != 1 || expectedGeneration[0] != p.ProjectGenerationID)) {
		return ErrForbidden
	}
	parentID := ""
	for _, binding := range p.AutoBindings {
		if binding.RegistrationID == event.TargetRegistrationID {
			parentID = binding.ParentID
		}
	}
	if parentID == "" {
		for _, token := range p.Tokens {
			if token.Nickname == event.TargetName {
				parentID = token.ParentID
			}
		}
	}
	for _, token := range p.Tokens {
		if token.ID == parentID {
			event.TargetParentLabel = token.Label
		}
	}
	data, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	return s.writeAuditBatch(data, []AuditEvent{{Version: 1, Project: project, OwnerID: actor, Kind: event.Kind, At: event.At, Management: &event}}, s.data.Projects)
}
