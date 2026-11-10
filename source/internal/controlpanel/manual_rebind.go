package controlpanel

import (
	"encoding/hex"
	"slices"

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type PendingRebind struct {
	Project string
	Journal protocol.RebindJournal
}

func validRebindJournals(entries []protocol.RebindJournal) bool {
	if len(entries) > projectTokenCap {
		return false
	}
	registrations, callers := map[string]bool{}, map[string]bool{}
	for _, j := range entries {
		source, err := hex.DecodeString(j.SourceDigest)
		if !safeDeviceText(j.RequestID, 128) || len(j.TokenID) != 52 || err != nil || len(source) != 32 || !registrationNickname.MatchString(j.RegistrationID) || j.ExpectedRevision == 0 || !validNickname(j.TemporaryNickname) || !validNickname(j.Name) || registrations[j.RegistrationID] || callers[j.TokenID] {
			return false
		}
		registrations[j.RegistrationID], callers[j.TokenID] = true, true
	}
	return true
}
func (s *Store) PendingRebinds() []PendingRebind {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []PendingRebind
	for project, p := range s.data.Projects {
		for _, j := range p.Rebinds {
			out = append(out, PendingRebind{project, j})
		}
	}
	return out
}

// RecoverRebind is used before serving requests. No raw credential is needed;
// the runtime supplies proof read from the registration named by the journal.
func (s *Store) RecoverRebind(project string, j protocol.RebindJournal, result protocol.RebindResult, applied bool) error {
	phase := "abort"
	if applied {
		phase = "confirm"
	}
	return s.manualRebind(project, j.TokenID, phase, j, result)
}
func (s *Store) manualRebind(project, caller, phase string, j protocol.RebindJournal, result protocol.RebindResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Projects[project]
	if s.closed || !ok || !p.Enabled || p.Deleting {
		return ErrForbidden
	}
	index := -1
	for i, t := range p.Tokens {
		if t.ID == caller {
			index = i
			break
		}
	}
	if index < 0 {
		return ErrForbidden
	}
	token := p.Tokens[index]
	if token.Kind != tokenSession || token.AdmissionMode == "auto" || token.BoundIdentity == "" || token.OwnerID == "" {
		return ErrForbidden
	}
	pending := -1
	for i, entry := range p.Rebinds {
		if entry.RequestID == j.RequestID && entry.TokenID == caller && entry.RegistrationID == j.RegistrationID && entry.ExpectedRevision == j.ExpectedRevision {
			pending = i
			break
		}
	}
	switch phase {
	case "pending":
		j.TokenID = caller
		j.TemporaryNickname = token.Nickname
		if !s.sessionTokenEligibleLocked(p, token) || !validRebindJournals([]protocol.RebindJournal{j}) {
			return ErrForbidden
		}
		if pending >= 0 {
			if p.Rebinds[pending] != j {
				return protocol.HandoverRefusal{Reason: "revision-mismatch"}
			}
			return nil
		}
		for _, entry := range p.Rebinds {
			if entry.TokenID == caller || entry.RegistrationID == j.RegistrationID {
				return protocol.HandoverRefusal{Reason: "revision-mismatch"}
			}
		}
		p.Rebinds = append(slices.Clone(p.Rebinds), j)
		return s.commitProject(p)
	case "abort":
		if pending < 0 {
			return nil
		}
		p.Rebinds = slices.Delete(slices.Clone(p.Rebinds), pending, pending+1)
		return s.commitProject(p)
	case "confirm":
		if pending < 0 {
			if result.RegistrationID == j.RegistrationID && token.Nickname == result.Name && !token.FirstConnectedAt.IsZero() {
				return nil
			}
			return ErrForbidden
		}
		entry := p.Rebinds[pending]
		if result.RegistrationID != entry.RegistrationID || result.Revision != entry.ExpectedRevision+1 || result.Name != entry.Name || result.HandoverFrom == nil {
			return ErrForbidden
		}
		p.Tokens = slices.Clone(p.Tokens)
		for i, old := range p.Tokens {
			if old.OwnerID == token.OwnerID && old.Kind == tokenSession && digest(old.BoundIdentity) == entry.SourceDigest {
				p.Tokens[i].Revoked = true
				p.Tokens[i].RevokedAt = s.now().UTC()
			}
		}
		for i, other := range p.Tokens {
			if i != index && tokenReservesName(other, result.Name) {
				return ErrNicknameTaken
			}
		}
		p.Tokens[index].Nickname = result.Name
		if p.Tokens[index].FirstConnectedAt.IsZero() {
			p.Tokens[index].FirstConnectedAt = s.now().UTC()
		}
		p.AutoBindings = slices.DeleteFunc(slices.Clone(p.AutoBindings), func(b AutoBinding) bool {
			return b.OwnerID == token.OwnerID && digest(b.Identity) == entry.SourceDigest
		})
		p.Rebinds = slices.Delete(slices.Clone(p.Rebinds), pending, pending+1)
		at := result.HandoverFrom.At
		event := AuditEvent{Version: 1, Project: project, OwnerID: token.OwnerID, Kind: "handover", At: at, Handover: &protocol.HandoverAudit{Previous: entry.SourceDigest, Current: digest(token.BoundIdentity), RegistrationID: result.RegistrationID, Name: result.Name, At: at}}
		if err := s.commitDirectProject(p, []AuditEvent{event}); err != nil {
			return err
		}
		s.notifyTokenChangesLocked(project, p.Tokens)
		return nil
	default:
		return ErrInvalid
	}
}
