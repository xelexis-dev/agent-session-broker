package controlpanel

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
)

// memberUsernames exposes only usernames to project managers. Permission and
// membership filtering use one snapshot so project changes cannot widen access.
func (s *Store) memberUsernames(actor, project, query string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.projectRoleLocked(actor, project) != "owner" {
		return nil, ErrForbidden
	}
	if len(query) > 64 {
		return nil, ErrInvalid
	}
	query = strings.ToLower(strings.TrimSpace(query))
	result := []string{}
	if query == "" {
		return result, nil
	}
	members := s.data.Projects[project].Members
	for id, user := range s.data.Users {
		if user.Enabled && members[id] == "" && strings.Contains(strings.ToLower(user.Username), query) {
			result = append(result, user.Username)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := strings.HasPrefix(strings.ToLower(result[i]), query), strings.HasPrefix(strings.ToLower(result[j]), query)
		if a != b {
			return a
		}
		return result[i] < result[j]
	})
	if len(result) > 10 {
		result = result[:10]
	}
	return result, nil
}

func (p *panel) memberSuggestions(w http.ResponseWriter, r *http.Request, session loginSession) {
	usernames, err := p.store.memberUsernames(session.userID, r.URL.Query().Get("project"), r.URL.Query().Get("q"))
	if errors.Is(err, ErrForbidden) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "invalid query", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Usernames []string `json:"usernames"`
	}{usernames})
}
