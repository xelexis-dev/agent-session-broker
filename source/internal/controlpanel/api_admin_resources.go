package controlpanel

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

type apiSessionList struct {
	Items []sessionRowDTO `json:"items"`
}
type apiSessionMutation struct {
	Generation *string `json:"generation,omitempty"`
}
type apiSuspensionRequest struct {
	Suspended  *bool   `json:"suspended"`
	Generation *string `json:"generation,omitempty"`
}
type apiDeviceRequest struct {
	UserCode string `json:"user_code"`
}
type apiDeviceDecision struct {
	UserCode string `json:"user_code"`
	Action   string `json:"action"`
	Nickname string `json:"nickname,omitempty"`
}
type apiDeviceView struct {
	Project     string    `json:"project"`
	OwnerID     string    `json:"owner_id"`
	Harness     string    `json:"harness"`
	InstanceID  string    `json:"instance_id"`
	SessionID   string    `json:"session_id"`
	ClientID    string    `json:"client_id"`
	CWDBasename string    `json:"cwd_basename"`
	Status      string    `json:"status"`
	Nickname    string    `json:"nickname"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func apiDeviceProjection(v DeviceView) apiDeviceView {
	return apiDeviceView{v.Project, v.OwnerID, v.Harness, v.InstanceID, v.SessionID, v.ClientID, v.CWDBasename, v.Status, v.Nickname, v.CreatedAt, v.ExpiresAt}
}
func readOptionalAPIBody(r *http.Request, target any) error {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return nil
	}
	return readAPIBody(r, target)
}
func (p *panel) apiProjectGuard(a apiPrincipal, project, scope string, generation *string) commitGuard {
	return func() error {
		if err := a.guardLocked(p.store, scope)(); err != nil {
			return err
		}
		role := p.store.projectRoleLocked(a.userID, project)
		if role == "" || scope == "write" && roleRank(role) < 2 {
			return ErrForbidden
		}
		if generation != nil && *generation != p.store.data.Projects[project].ProjectGenerationID {
			return errProjectGenerationMismatch
		}
		return nil
	}
}
func (p *panel) apiSessions(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	project := r.PathValue("project")
	release, err := p.store.startProjectOperationGuarded(project, nil, p.apiProjectGuard(a, project, "read", nil))
	if err != nil {
		apiError(w, err)
		return
	}
	defer release()
	if p.backend == nil {
		apiError(w, ErrStorage)
		return
	}
	sessions, err := p.backend.Sessions(r.Context(), project)
	if err != nil {
		apiError(w, ErrStorage)
		return
	}
	rows, err := p.store.sessionProjectionsFor(a.userID, project, sessions)
	if err != nil {
		apiError(w, err)
		return
	}
	apiJSON(w, 200, apiSessionList{rows})
}
func (p *panel) apiSessionChange(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiSessionMutation
	op, id, grant, route := "/sessions/detach", r.PathValue("registrationId"), "", "POST /api/v1/projects/{project}/sessions/{registrationId}/detach"
	if r.Method == "PUT" {
		var change apiSuspensionRequest
		if err := readAPIBody(r, &change); err != nil {
			apiError(w, err)
			return
		}
		if change.Suspended == nil {
			apiError(w, ErrInvalid)
			return
		}
		req.Generation = change.Generation
		op = "/sessions/unsuspend"
		if *change.Suspended {
			op = "/sessions/suspend"
		}
		route = "PUT /api/v1/projects/{project}/sessions/{registrationId}/suspension"
	} else {
		if err := readOptionalAPIBody(r, &req); err != nil {
			apiError(w, err)
			return
		}
		if r.PathValue("id") != "" {
			id = ""
			grant = r.PathValue("id")
			route = "POST /api/v1/projects/{project}/reserved-grants/{id}/release"
		}
	}
	if p.backend == nil {
		apiError(w, ErrStorage)
		return
	}
	project := r.PathValue("project")
	if err := p.authorizeMutation(a.userID, op, project); err != nil {
		apiError(w, err)
		return
	}
	_, err := p.manageSession(r.Context(), a.userID, project, op, id, grant, p.apiProjectGuard(a, project, "write", req.Generation), id != "")
	if err != nil {
		var failure *sessionMutationError
		if errors.As(err, &failure) {
			if failure.status == 409 {
				apiJSON(w, 409, apiErrorView{"session_conflict", "The session changed or could not be managed."})
				return
			}
			switch {
			case failure.cause == nil:
				err = ErrInvalid
			case errors.Is(failure.cause, ErrForbidden), errors.Is(failure.cause, ErrInvalid):
				err = failure.cause
			default:
				err = ErrStorage
			}
		}
		apiError(w, err)
		return
	}
	p.store.apiAuditAfter(route, a.userID, project, "")
	w.WriteHeader(204)
}
func (p *panel) apiDeviceLookup(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiDeviceRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	view, err := p.decideDeviceCode(a.userID, req.UserCode, "lookup", "", a.guardLocked(p.store, "approve"))
	if err != nil {
		apiError(w, err)
		return
	}
	apiJSON(w, 200, apiDeviceProjection(view))
}
func (p *panel) apiDeviceDecision(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiDeviceDecision
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	if req.Action != "approve" && req.Action != "deny" {
		apiError(w, ErrInvalid)
		return
	}
	view, err := p.decideDeviceCode(a.userID, req.UserCode, req.Action, req.Nickname, a.guardLocked(p.store, "approve"))
	if err != nil {
		apiError(w, err)
		return
	}
	p.store.apiAuditAfter("POST /api/v1/device-approvals/decision", a.userID, view.Project, "")
	apiJSON(w, 200, apiDeviceProjection(view))
}

type apiAuditCursor struct {
	V        int       `json:"v"`
	Resource string    `json:"resource"`
	Offset   int       `json:"offset"`
	Before   time.Time `json:"before"`
}
type apiProjectAuditPage struct {
	Items      []projectAuditDTO `json:"items"`
	NextCursor string            `json:"next_cursor"`
}
type apiAuditDTO struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Actor  string    `json:"actor"`
	Target string    `json:"target"`
	Role   string    `json:"role"`
	Result string    `json:"result"`
	Count  int       `json:"count"`
}
type apiAdminAuditPage struct {
	Items      []any  `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func apiAuditWindow(r *http.Request, resource string, now time.Time) (apiAuditCursor, int, error) {
	cursor := apiAuditCursor{V: 1, Resource: resource, Before: now.UTC()}
	limit := 100
	q, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil {
		return cursor, 0, ErrInvalid
	}
	for k, v := range q {
		if len(v) != 1 || (k != "limit" && k != "cursor" && (k != "kind" || !strings.HasPrefix(resource, "admin:"))) {
			return cursor, 0, ErrInvalid
		}
	}
	if v, ok := q["limit"]; ok {
		n, e := strconv.Atoi(v[0])
		if e != nil || n < 1 || n > 200 {
			return cursor, 0, ErrInvalid
		}
		limit = n
	}
	if v, ok := q["cursor"]; ok {
		if len(v[0]) > 1024 {
			return cursor, 0, ErrInvalid
		}
		raw, e := base64.RawURLEncoding.DecodeString(v[0])
		if e != nil || strictAPIBytes(raw, &cursor) != nil || cursor.V != 1 || cursor.Resource != resource || cursor.Offset < 0 || cursor.Offset > maxAuditEvents || cursor.Before.IsZero() || cursor.Before.After(now) {
			return cursor, 0, ErrInvalid
		}
	}
	return cursor, limit, nil
}
func nextAuditCursor(c apiAuditCursor, end, total int) string {
	if end >= total {
		return ""
	}
	c.Offset = end
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}
func (p *panel) apiProjectAudit(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	project := r.PathValue("project")
	s := p.store
	s.mu.RLock()
	err := p.apiProjectGuard(a, project, "read", nil)()
	now := s.now()
	s.mu.RUnlock()
	if err != nil {
		apiError(w, err)
		return
	}
	cursor, limit, err := apiAuditWindow(r, "project:"+project+":"+a.userID, now)
	if err != nil {
		apiError(w, err)
		return
	}
	view, err := s.directAuditViewWindowFor(a.userID, project, "current", i18n.English, cursor.Before, maxAuditEvents)
	if err != nil {
		apiError(w, err)
		return
	}
	if cursor.Offset > len(view.Rows) {
		apiError(w, ErrInvalid)
		return
	}
	end := min(cursor.Offset+limit, len(view.Rows))
	out := make([]projectAuditDTO, 0, end-cursor.Offset)
	for _, row := range view.Rows[cursor.Offset:end] {
		out = append(out, projectAuditProjection(row))
	}
	apiJSON(w, 200, apiProjectAuditPage{out, nextAuditCursor(cursor, end, len(view.Rows))})
}
func (p *panel) apiAdminAudit(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	kind := r.URL.Query().Get("kind")
	if kind != "api" && kind != "registration" {
		apiError(w, ErrInvalid)
		return
	}
	s := p.store
	s.mu.RLock()
	err := a.guardLocked(s, "admin")()
	now := s.now()
	s.mu.RUnlock()
	if err != nil {
		apiError(w, err)
		return
	}
	cursor, limit, err := apiAuditWindow(r, "admin:"+kind+":"+a.userID, now)
	if err != nil {
		apiError(w, err)
		return
	}
	rows := make([]any, 0)
	if kind == "registration" {
		view, e := s.directAuditViewWindowFor(a.userID, "", "current", i18n.English, cursor.Before, maxAuditEvents)
		if e != nil {
			apiError(w, e)
			return
		}
		for _, row := range view.Rows {
			rows = append(rows, registrationAuditProjection(row))
		}
	} else {
		s.mu.RLock()
		if err := a.guardLocked(s, "admin")(); err != nil {
			s.mu.RUnlock()
			apiError(w, err)
			return
		}
		keys := make([]string, 0, len(s.api.audits))
		for k, e := range s.api.audits {
			if !e.At.After(cursor.Before) {
				keys = append(keys, k)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		for _, key := range keys {
			e := s.api.audits[key]
			rows = append(rows, apiAuditDTO{e.At, e.Action, e.Actor, e.Target, e.Role, e.Result, e.Count})
		}
		s.mu.RUnlock()
	}
	if cursor.Offset > len(rows) {
		apiError(w, ErrInvalid)
		return
	}
	end := min(cursor.Offset+limit, len(rows))
	apiJSON(w, 200, apiAdminAuditPage{rows[cursor.Offset:end], nextAuditCursor(cursor, end, len(rows))})
}

func adminResourceRoutes() []apiRoute {
	return append([]apiRoute{
		{"GET", "/api/v1/projects/{project}/sessions", "read", "admin-api-sessions", nil, reflect.TypeFor[apiSessionList](), 200, (*panel).apiSessions},
		{"POST", "/api/v1/projects/{project}/sessions/{registrationId}/detach", "write", "admin-api-detach", reflect.TypeFor[apiSessionMutation](), nil, 204, (*panel).apiSessionChange},
		{"PUT", "/api/v1/projects/{project}/sessions/{registrationId}/suspension", "write", "admin-api-suspension", reflect.TypeFor[apiSuspensionRequest](), nil, 204, (*panel).apiSessionChange},
		{"POST", "/api/v1/projects/{project}/reserved-grants/{id}/release", "write", "admin-api-release", reflect.TypeFor[apiSessionMutation](), nil, 204, (*panel).apiSessionChange},
		{"POST", "/api/v1/device-approvals/lookup", "approve", "admin-api-device-lookup", reflect.TypeFor[apiDeviceRequest](), reflect.TypeFor[apiDeviceView](), 200, (*panel).apiDeviceLookup},
		{"POST", "/api/v1/device-approvals/decision", "approve", "admin-api-device-decision", reflect.TypeFor[apiDeviceDecision](), reflect.TypeFor[apiDeviceView](), 200, (*panel).apiDeviceDecision},
		{"GET", "/api/v1/projects/{project}/audit", "read", "admin-api-project-audit", nil, reflect.TypeFor[apiProjectAuditPage](), 200, (*panel).apiProjectAudit},
		{"GET", "/api/v1/admin/audit", "admin", "admin-api-admin-audit", nil, reflect.TypeFor[apiAdminAuditPage](), 200, (*panel).apiAdminAudit},
	}, adminSettingsRoutes()...)
}
