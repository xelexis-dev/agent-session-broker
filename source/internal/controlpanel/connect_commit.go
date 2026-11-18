package controlpanel

import (
	"crypto/rand"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"maps"
	"slices"
	"strings"
)

// prepareConnectCommit works on private copies: failed batches publish neither
// token changes nor child revocations, and never send cancellation notices.
func (s *Store) prepareConnectCommit(candidate state, tokens map[string]userAPIToken, events []connectAudit) (state, map[string]userAPIToken, []connectAudit) {
	if tokens == nil {
		tokens = maps.Clone(s.api.tokens)
	}
	candidate.Projects = maps.Clone(candidate.Projects)
	now := s.now()
	reasons := map[string]string{}
	for id, old := range s.api.tokens {
		if !isConnectToken(old) {
			continue
		}
		t, present := tokens[id]
		reason := ""
		if !present {
			reason = "explicit"
		} else if !now.Before(t.ExpiresAt) {
			reason = "expired"
		} else if !connectTokenLive(candidate, t, now) {
			reason = "user-changed"
		}
		if reason != "" {
			delete(tokens, id)
			recorded := false
			for _, event := range events {
				if event.Kind == "token-revoked" && event.ParentID == id {
					reason = event.Reason
					recorded = true
					break
				}
			}
			reasons[id] = reason
			if !recorded {
				events = append(events, connectEvent(old, Project{}, "", "token-revoked", reason, ""))
			}
			continue
		}
		t = cloneConnectToken(t)
		kept := []connectProject{}
		for _, ref := range t.Connect.AutoProjects {
			p, ok := candidate.Projects[ref.Name]
			oldP := s.data.Projects[ref.Name]
			lost := !ok || p.Deleting || p.ProjectGenerationID != ref.Generation || p.Members[t.User] != "owner" || roleRank(p.Members[t.User]) < roleRank(oldP.Members[t.User])
			if lost {
				events = append(events, connectEvent(t, Project{Name: ref.Name, ProjectGenerationID: ref.Generation}, "", "auto-project-removed", "membership-lost", ""))
			} else {
				kept = append(kept, ref)
			}
		}
		t.Connect.AutoProjects = kept
		tokens[id] = t
	}
	for name, p := range candidate.Projects {
		p.Tokens = slices.Clone(p.Tokens)
		oldP := s.data.Projects[name]
		for i, g := range p.Tokens {
			if g.Kind != tokenSession {
				continue
			}
			parent, known := s.api.tokens[g.ParentID]
			if !known {
				parent, known = tokens[g.ParentID]
			}
			if !known || !isConnectToken(parent) {
				continue
			}
			reason := reasons[g.ParentID]
			if reason == "" && (p.Deleting || oldP.ProjectGenerationID != "" && oldP.ProjectGenerationID != p.ProjectGenerationID) {
				reason = "project-deleted"
			}
			if reason == "" && (roleRank(p.Members[parent.User]) < roleRank("developer") || roleRank(p.Members[parent.User]) < roleRank(oldP.Members[parent.User])) {
				reason = "membership-lost"
			}
			if reason == "" && g.AdmissionMode == "auto" {
				t, ok := tokens[g.ParentID]
				if !ok || t.Connect.AdmissionMode != "auto" || p.Members[parent.User] != "owner" || !slices.Contains(t.Connect.AutoProjects, connectProject{name, p.ProjectGenerationID}) {
					reason = "membership-lost"
				}
			}
			if reason != "" {
				p.Tokens[i].Revoked = true
			}
		}
		candidate.Projects[name] = p
	}
	// Compare old and new grants so explicit revoke, project deletion, and sweeps
	// share the same audit path, including deletions that remove every token.
	for name, oldP := range s.data.Projects {
		p, present := candidate.Projects[name]
		for _, g := range oldP.Tokens {
			parent, known := s.api.tokens[g.ParentID]
			if !known || !isConnectToken(parent) || g.Revoked || !g.ExpiredAt.IsZero() {
				continue
			}
			var next *SessionToken
			for i := range p.Tokens {
				if p.Tokens[i].ID == g.ID {
					next = &p.Tokens[i]
					break
				}
			}
			if next != nil && !next.Revoked && next.ExpiredAt.IsZero() {
				continue
			}
			reason := reasons[parent.ID]
			if reason == "" && tokenExpired(g, now) {
				reason = "expired"
			}
			if reason == "" {
				for _, b := range oldP.AutoBindings {
					if b.GrantID == g.ID && b.State == "issued" && !now.Before(b.ExpiresAt) {
						reason = "expired"
					}
				}
			}
			if reason == "" {
				reason = "explicit"
			}
			if !present || p.Deleting || p.ProjectGenerationID != oldP.ProjectGenerationID {
				reason = "project-deleted"
			} else if roleRank(p.Members[parent.User]) < roleRank(oldP.Members[parent.User]) {
				reason = "membership-lost"
			}
			events = append(events, connectEvent(parent, oldP, g.ID, "grant-revoked", reason, ""))
		}
	}
	for name, p := range candidate.Projects {
		old := map[string]bool{}
		for _, g := range s.data.Projects[name].Tokens {
			old[g.ID] = true
		}
		for _, g := range p.Tokens {
			parent, ok := tokens[g.ParentID]
			if !old[g.ID] && ok && isConnectToken(parent) && !g.Revoked {
				reason, actor := "approved", ""
				if g.AdmissionMode == "auto" {
					reason = "auto"
				} else {
					for _, d := range p.Devices {
						if d.GrantID == g.ID {
							actor = d.ApprovedBy
							break
						}
					}
				}
				events = append(events, connectEvent(parent, p, g.ID, "grant-issued", reason, actor))
			}
		}
	}
	// Parent revocation summarizes the cascade; record it after the children so
	// one large cascade cannot evict its own newest parent-revoked audit row.
	summaries := []connectAudit{}
	kept := events[:0]
	for _, event := range events {
		if event.Kind == "token-revoked" {
			summaries = append(summaries, event)
		} else {
			kept = append(kept, event)
		}
	}
	events = append(kept, summaries...)
	return candidate, tokens, events
}

func (s *Store) connectRowsLocked(tokens map[string]userAPIToken, events []connectAudit) (map[string][]byte, []string, map[string]connectAudit, error) {
	puts := map[string][]byte{}
	var deletes []string
	for id, t := range s.api.tokens {
		if isConnectToken(t) {
			if _, ok := tokens[id]; !ok {
				deletes = append(deletes, t.key())
			}
		}
	}
	for _, t := range tokens {
		if !isConnectToken(t) {
			continue
		}
		raw, _ := json.Marshal(t)
		old, _ := json.Marshal(s.api.tokens[t.ID])
		if string(raw) != string(old) {
			puts[t.key()] = raw
		}
	}
	audits := maps.Clone(s.connect.audits)
	if audits == nil {
		audits = map[string]connectAudit{}
	}
	for key, e := range audits {
		if !s.now().Before(e.At.Add(auditRetention)) {
			delete(audits, key)
			deletes = append(deletes, key)
		}
	}

	// Quotas evict the flooding principal's own oldest records before the global
	// cap. New records follow existing records when a test clock gives equal times.
	added := map[string]int{}
	for i, e := range events {
		e.At = s.now().UTC()
		if !validConnectAudit(e) {
			return nil, nil, nil, ErrStorage
		}
		key := "connect-audit/" + rand.Text()
		raw, _ := json.Marshal(e)
		puts[key] = raw
		audits[key] = e
		added[key] = i + 1
	}
	keys := make([]string, 0, len(audits))

	parentCounts, userCounts := map[string]int{}, map[string]int{}
	auditUser := func(e connectAudit) string {
		if e.UserID != "" {
			return e.UserID
		}
		return e.ActorID
	}
	for key, e := range audits {
		keys = append(keys, key)
		if e.ParentID != "" {
			parentCounts[e.ParentID]++
		}
		userCounts[auditUser(e)]++
	}

	slices.SortFunc(keys, func(a, b string) int {
		if c := audits[a].At.Compare(audits[b].At); c != 0 {
			return c
		}
		if added[a] < added[b] {
			return -1
		}
		if added[a] > added[b] {
			return 1
		}
		return strings.Compare(a, b)
	})
	drop := func(key string) {
		delete(audits, key)
		delete(puts, key)
		if _, exists := s.connect.audits[key]; exists {
			deletes = append(deletes, key)
		}
	}

	for _, key := range keys {
		e := audits[key]
		if e.ParentID != "" && parentCounts[e.ParentID] > maxConnectAuditPerParent {
			drop(key)
			parentCounts[e.ParentID]--
			userCounts[auditUser(e)]--
		}
	}
	for _, key := range keys {
		if e, exists := audits[key]; exists && userCounts[auditUser(e)] > maxConnectAuditPerUser {
			drop(key)
			userCounts[auditUser(e)]--
		}
	}

	for _, key := range keys {
		if len(audits) <= maxConnectAudit {
			break
		}
		if _, exists := audits[key]; exists {
			drop(key)
		}
	}

	return puts, deletes, audits, nil
}

func (s *Store) connectCapacityLocked(candidate state, tokens map[string]userAPIToken) error {
	for name, p := range candidate.Projects {
		old := map[string]bool{}
		for _, g := range s.data.Projects[name].Tokens {
			old[g.ID] = true
		}
		for _, g := range p.Tokens {
			parent, ok := tokens[g.ParentID]
			if old[g.ID] {
				continue
			}
			if !ok {
				if previous, exists := s.api.tokens[g.ParentID]; exists && isConnectToken(previous) {
					return errConnectForbidden
				}
				continue
			}
			if !isConnectToken(parent) {
				continue
			}
			if !s.connect.enabled {
				return errConnectDisabled
			}
			if !connectTokenLive(candidate, parent, s.now()) || g.Revoked || !p.Enabled || p.Deleting || p.Mode != "token" || g.OwnerID != parent.User || roleRank(p.Members[parent.User]) < roleRank("developer") || !access.ScopesSubset(g.Scopes, access.DelegatedScopes(parent.Connect.MCPScopes)) {
				return errConnectForbidden
			}
			if g.AdmissionMode == "auto" && (parent.Connect.AdmissionMode != "auto" || p.Members[parent.User] != "owner" || !slices.Contains(parent.Connect.AutoProjects, connectProject{name, p.ProjectGenerationID})) {
				return errConnectForbidden
			}
			for _, t := range p.Tokens {
				if t.ID == parent.ID && t.Kind == tokenProject {
					return errConnectForbidden
				}
			}
		}
	}
	for id, t := range tokens {
		if !isConnectToken(t) {
			continue
		}
		count := connectCount(candidate, id, s.now())
		if count > t.Connect.MaxSessions && count > connectCount(s.data, id, s.now()) {
			return errConnectCapacity
		}
	}
	return nil
}
