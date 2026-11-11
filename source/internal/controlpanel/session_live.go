package controlpanel

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
	"net/http"
	"time"
)

type liveSessionRow struct {
	Key             string          `json:"key"`
	Shape           string          `json:"shape"`
	DisplayState    string          `json:"displayState"`
	ConnectionLabel string          `json:"connectionLabel"`
	Status          string          `json:"status"`
	Reason          string          `json:"reason"`
	ProbePending    bool            `json:"probePending"`
	CanManage       bool            `json:"canManage"`
	RuntimeInfo     runtimeInfoView `json:"runtimeInfo"`
	Activity        activityView    `json:"activity"`
}

func liveJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "JSON encoding failed", http.StatusInternalServerError)
		return
	}
	writePanelText(w, r, status, append(data, '\n'))
}
func (p *panel) liveSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		liveJSON(w, r, 405, map[string]string{"error": "method-not-allowed"})
		return
	}
	session, ok := p.session(r)
	if !ok || !p.store.Ready() {
		liveJSON(w, r, 401, map[string]string{"error": "authentication-required"})
		return
	}
	project := r.URL.Query().Get("project")
	if len(r.URL.Query()["project"]) != 1 || !projectName.MatchString(project) {
		liveJSON(w, r, 400, map[string]string{"error": "invalid-project"})
		return
	}
	exists, role := p.store.liveProjectAccess(session.userID, project)
	if !exists {
		liveJSON(w, r, 404, map[string]string{"error": "project-not-found"})
		return
	}
	if role == "" {
		liveJSON(w, r, 403, map[string]string{"error": "forbidden"})
		return
	}
	if p.backend == nil {
		liveJSON(w, r, 503, map[string]string{"error": "session-list-unavailable"})
		return
	}
	release, err := p.store.startProjectOperation(project, nil)
	if err != nil {
		liveJSON(w, r, 503, map[string]string{"error": "session-list-unavailable"})
		return
	}
	defer release()
	var sessions []Session
	if reader, ok := p.backend.(interface {
		LiveSessions(context.Context, string) ([]Session, error)
	}); ok {
		sessions, err = reader.LiveSessions(r.Context(), project)
	} else {
		sessions, err = p.backend.Sessions(r.Context(), project)
	}
	if err != nil {
		liveJSON(w, r, 503, map[string]string{"error": "session-list-unavailable"})
		return
	}
	// Membership/account may have changed while the backend was being read.
	current, ok := p.session(r)
	if !ok {
		liveJSON(w, r, 401, map[string]string{"error": "authentication-required"})
		return
	}
	if p.store.ProjectRole(current.userID, project) == "" {
		liveJSON(w, r, 403, map[string]string{"error": "forbidden"})
		return
	}
	language := liveLanguage(r)
	rows, summary, err := p.store.liveSessionRows(current.userID, project, language, sessions)
	if err != nil {
		liveJSON(w, r, 403, map[string]string{"error": "forbidden"})
		return
	}

	liveJSON(w, r, 200, struct {
		Project    string           `json:"project"`
		Language   string           `json:"language"`
		ServerTime string           `json:"serverTime"`
		Rows       []liveSessionRow `json:"rows"`
		Summary    sessionSummary   `json:"summary"`
	}{project, language, activityTime(time.Now()), rows, summary})
}

// liveLanguage is the language of the page that polls: exactly one lang=en|ko. Anything else means a tab opened
// before 1.1, which is Korean. It only selects display text; access was decided above.
func liveLanguage(r *http.Request) string {
	if values := r.URL.Query()["lang"]; len(values) == 1 && i18n.Normalize(values[0]) != "" {
		return values[0]
	}
	return i18n.Korean
}

// Polling must not run the ordinary page's persistent expiration housekeeping.
func (s *Store) liveProjectAccess(actor, project string) (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.data.Projects[project]
	return ok && !s.closed && !p.Deleting, s.projectRoleLocked(actor, project)
}
func (s *Store) liveSessionRows(actor, project, language string, sessions []Session) ([]liveSessionRow, sessionSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.data.Projects[project]
	if !ok || p.Deleting || s.projectRoleLocked(actor, project) == "" {
		return nil, sessionSummary{}, ErrForbidden
	}
	// Normalize expiration on a private copy, using the same policy as expireLocked.
	now := s.now().UTC()
	tokens := make([]SessionToken, 0, len(p.Tokens))
	for _, token := range p.Tokens {
		if token.Revoked && token.RevokedAt.IsZero() {
			token.RevokedAt = now
		}
		if token.ExpiredAt.IsZero() && tokenExpired(token, now) {
			token.ExpiredAt = tokenExpiry(token)
		}
		retired := token.ExpiredAt
		if !token.RevokedAt.IsZero() && (retired.IsZero() || token.RevokedAt.Before(retired)) {
			retired = token.RevokedAt
		}
		if !retired.IsZero() && !now.Before(retired.Add(tokenHistoryRetention)) {
			continue
		}
		tokens = append(tokens, token)
	}
	p.Tokens = tokens
	snapshot := Store{data: s.data}
	snapshot.data.Projects = map[string]Project{project: p}
	owners := snapshot.sessionOwnersLocked(project, sessions)
	all := append(append([]Session(nil), sessions...), reservedSessionRows(p, s.data.Users, sessions)...)
	rows := make([]liveSessionRow, 0, len(all))
	for _, item := range all {
		manage := snapshot.canManageSessionLocked(actor, project, sessionIdentity(item), item.OwnerID)
		owner := owners[sessionIdentity(item)]
		if item.GrantID != "" {
			manage = snapshot.canReleaseReservedGrantLocked(actor, project, item.GrantID)
			owner = item.GrantOwner
		}
		rows = append(rows, liveSessionRow{Key: item.RowKey(), Shape: item.RowShape(manage, owner), DisplayState: item.DisplayState(), ConnectionLabel: item.ConnectionLabel().In(language), Status: item.Status, Reason: item.StateReason().In(language), ProbePending: item.ProbePending && item.ErrorCode != "probe-unanswered", CanManage: manage, Activity: item.ActivityView(language), RuntimeInfo: item.RuntimeInfoView(language)})
	}
	return rows, summarizeSessions(all), nil
}
