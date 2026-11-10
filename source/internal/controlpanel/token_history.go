package controlpanel

import (
	"maps"
	"time"
)

// backfillTokenHistory derives the aggregate history of project tokens issued
// before this release from the session grants that still exist. It only
// touches tokens without any recorded history, so a token counted once is
// never counted again, and it never invents history for grants that have
// already expired and been removed.
func (s *Store) backfillTokenHistory() error {
	candidate := s.data
	candidate.Projects = maps.Clone(s.data.Projects)
	changed := false
	for name, p := range s.data.Projects {
		tokens := append([]SessionToken(nil), p.Tokens...)
		touched := false
		for i, token := range tokens {
			if token.Kind != tokenProject || token.Approvals != 0 || !token.LastRegisteredAt.IsZero() {
				continue
			}
			approvals, registered, used := 0, time.Time{}, token.LastUsedAt
			for _, grant := range p.Tokens {
				if grant.AdmissionMode == "auto" || grant.ParentID != token.ID {
					continue
				}
				approvals++
				if grant.FirstConnectedAt.After(registered) {
					registered = grant.FirstConnectedAt
				}
				if grant.LastUsedAt.After(used) {
					used = grant.LastUsedAt
				}
			}
			if approvals == 0 {
				continue
			}
			tokens[i].Approvals = approvals
			tokens[i].LastRegisteredAt = registered
			tokens[i].LastUsedAt = used
			touched = true
		}
		if touched {
			p.Tokens = tokens
			candidate.Projects[name] = p
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.commit(candidate)
}
