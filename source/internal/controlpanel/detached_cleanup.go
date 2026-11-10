package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"slices"
)

// Later proofs observe earlier mutations; policy and audit are committed together.
func (s *Store) CleanupDetachedAutoBatch(project, generation string, proofs []access.DetachedRemovalProof) error {
	valid := false
	for _, proof := range proofs {
		if validDetachedCleanupProof(proof) {
			valid = true
			break
		}
	}
	if !valid {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStorage
	}
	p, ok := s.data.Projects[project]
	if !ok || p.Deleting || generation == "" || p.ProjectGenerationID != generation {
		return nil
	}
	var events []AuditEvent
	for _, r := range proofs {
		var added []AuditEvent
		p, added = s.cleanupDetachedAutoLocked(p, r)
		events = append(events, added...)
	}
	if len(events) == 0 {
		return nil
	}
	if err := s.commitDirectProject(p, events); err != nil {
		return err
	}
	s.notifyTokenChangesLocked(project, p.Tokens)
	s.changed(project)
	return nil
}

func (s *Store) cleanupDetachedAutoLocked(p Project, r access.DetachedRemovalProof) (Project, []AuditEvent) {
	if !validDetachedCleanupProof(r) {
		return p, nil
	}
	index := -1
	for i, t := range p.Tokens {
		if t.ID != r.GrantID {
			continue
		}
		if t.Kind != tokenSession || t.AdmissionMode != "auto" || t.ParentID != r.ParentID || t.BoundIdentity != r.Identity || t.OwnerID != r.OwnerID || t.Nickname != r.Name || t.PendingNickname != "" {
			return p, nil
		}
		index = i
		break
	}
	var selected *AutoBinding
	for _, b := range p.AutoBindings {
		if b.GrantID != r.GrantID {
			if b.Identity == r.Identity || b.HandoverSource == r.Identity && b.State != "confirmed" {
				return p, nil
			}
			continue
		}
		if selected != nil || b.ParentID != r.ParentID || b.Identity != r.Identity || b.OwnerID != r.OwnerID || b.State != "confirmed" || b.RegistrationID != r.RegistrationID || b.DetachPending || b.PendingRevision != 0 {
			return p, nil
		}
		copy := b
		selected = &copy
	}
	if selected == nil {
		return p, nil
	}
	// The exact binding outlives token expiry/history pruning; release it too.
	if index >= 0 && tokenActive(p.Tokens[index], s.now()) {
		p.Tokens = slices.Clone(p.Tokens)
		p.Tokens[index].Revoked = true
	}
	p, events := s.releaseDirectGrantLocked(p, r.GrantID)
	p.AutoExchanges = slices.DeleteFunc(slices.Clone(p.AutoExchanges), func(e AutoExchange) bool { return e.GrantID == r.GrantID })
	return p, events
}

func validDetachedCleanupProof(r access.DetachedRemovalProof) bool {
	return r.Detached && r.ReattachAfterCleanup && r.Revision != 0 && r.RegistrationID != "" && r.Identity != "" && r.OwnerID != "" && r.AdmissionMode == "auto" && r.GrantID != "" && r.ParentID != ""
}
