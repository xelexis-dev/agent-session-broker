package controlpanel

import (
	"errors"
	"net/http"
	"reflect"
	"sort"
	"time"
)

type apiUserCreateRequest struct {
	Username        string `json:"username"`
	Password        string `json:"password"`
	Role            string `json:"role"`
	CurrentPassword string `json:"current_password"`
}
type apiUserUpdateRequest struct {
	Role            *string `json:"role,omitempty"`
	Enabled         *bool   `json:"enabled,omitempty"`
	Password        *string `json:"password,omitempty"`
	CurrentPassword string  `json:"current_password,omitempty"`
	Version         *string `json:"version,omitempty"`
}
type apiUserList struct {
	Items []apiMeView `json:"items"`
}

func (s *Store) apiAdminUserView(u User) apiMeView {
	v := s.apiUserView(u)
	v.Version = userSettingsVersion(u)
	return v
}

// apiPasswordProof holds one KDF slot until release, never Store.mu during KDF.
// The caller's final commit guard pins the revision that proved the password.
func (s *Store) apiPasswordProof(a apiPrincipal, password, scope string, needSlot bool) (release func(), verified bool, err error) {
	release = func() {}
	if len(password) > 1024 {
		return release, false, ErrInvalid
	}
	s.mu.Lock()
	if err = a.guardLocked(s, scope)(); err != nil {
		s.mu.Unlock()
		return release, false, err
	}
	if !needSlot && password == "" {
		s.mu.Unlock()
		return release, false, nil
	}
	select {
	case s.hashSlot <- struct{}{}:
	default:
		s.mu.Unlock()
		return release, false, errAPIBusy
	}
	release = func() { <-s.hashSlot }
	if password == "" {
		s.mu.Unlock()
		return release, false, nil
	}
	if !apiReauthAvailable(s.api.reauth, a.tokenID, s.now()) {
		s.mu.Unlock()
		release()
		return func() {}, false, errAPIRate
	}
	verifier := s.data.Users[a.userID].Password
	s.mu.Unlock()
	verified = verifyAPISecret(verifier, password)
	s.mu.Lock()
	err = a.guardLocked(s, scope)()
	if err == nil && !verified {
		rateAllowed(s.api.reauth, a.tokenID, s.now(), 5, 15*time.Minute)
		s.apiRefusalLocked("reauth_failed")
		err = errAPICredentials
	}
	s.mu.Unlock()
	if err != nil {
		release()
		return func() {}, false, err
	}
	return release, verified, nil
}

func (p *panel) apiAdminUsers(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	s := p.store
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := a.guardLocked(s, "admin")(); err != nil {
		apiError(w, err)
		return
	}
	out := make([]apiMeView, 0, len(s.data.Users))
	for _, u := range s.data.Users {
		out = append(out, s.apiAdminUserView(u.User))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	apiJSON(w, 200, apiUserList{out})
}
func (p *panel) apiAdminUser(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	s := p.store
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := a.guardLocked(s, "admin")(); err != nil {
		apiError(w, err)
		return
	}
	u, ok := s.data.Users[r.PathValue("id")]
	if !ok {
		apiError(w, errAPINotFound)
		return
	}
	apiJSON(w, 200, s.apiAdminUserView(u.User))
}
func (p *panel) apiAdminUserCreate(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiUserCreateRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	if req.CurrentPassword == "" {
		apiError(w, ErrForbidden)
		return
	}
	release, verified, err := p.store.apiPasswordProof(a, req.CurrentPassword, "admin", true)
	if err != nil {
		apiError(w, err)
		return
	}
	defer release()
	if !verified {
		apiError(w, ErrForbidden)
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}
	s := p.store
	s.mu.Lock()
	err = s.createUserLocked(a.userID, req.Username, req.Password, req.Role, a.guardLocked(s, "admin"))
	var view apiMeView
	if err == nil {
		view = s.apiAdminUserView(s.data.Users[req.Username].User)
	}
	s.mu.Unlock()
	if err != nil {
		apiError(w, err)
		return
	}
	s.apiAuditAfter("POST /api/v1/admin/users", a.userID, req.Username, req.Role)
	apiJSON(w, 201, view)
}
func (p *panel) apiAdminUserUpdate(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiUserUpdateRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	if req.Password != nil && (len(*req.Password) < 12 || len(*req.Password) > 1024) {
		apiError(w, ErrInvalid)
		return
	}
	release, verified, err := p.store.apiPasswordProof(a, req.CurrentPassword, "admin", true)
	if err != nil {
		apiError(w, err)
		return
	}
	defer release()
	s := p.store
	id := r.PathValue("id")
	s.mu.Lock()
	old, ok := s.data.Users[id]
	if !ok {
		s.mu.Unlock()
		apiError(w, errAPINotFound)
		return
	}
	role, enabled, password := old.Role, old.Enabled, ""
	if req.Role != nil {
		role = *req.Role
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if req.Password != nil {
		password = *req.Password
	}
	changed, reduces := false, false
	guard := func(c userChange) error {
		if err := a.guardLocked(s, "admin")(); err != nil {
			return err
		}
		if c.RaisesPower && !verified {
			return ErrForbidden
		}
		changed = c.Before.Revision != c.After.Revision
		reduces = (c.Before.Enabled && !c.After.Enabled) || (c.Before.Role == "admin" && c.After.Role != "admin") || c.PasswordSet
		return nil
	}
	err = s.updateUserLocked(a.userID, id, role, enabled, password, guard, apiVersion(req.Version)...)
	if (err == nil && changed) || (errors.Is(err, ErrStorage) && reduces) {
		_ = s.invalidateAPITokensLocked(id)
	}
	var view apiMeView
	if err == nil {
		view = s.apiAdminUserView(s.data.Users[id].User)
	}
	s.mu.Unlock()
	if err != nil {
		apiError(w, err)
		return
	}
	auditRole := ""
	if old.Role != view.Role {
		auditRole = view.Role
	}
	s.apiAuditAfter("PATCH /api/v1/admin/users/{id}", a.userID, id, auditRole)
	apiJSON(w, 200, view)
}
func (p *panel) apiAdminUserTokens(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	s := p.store
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := a.guardLocked(s, "admin")(); err != nil {
		apiError(w, err)
		return
	}
	id := r.PathValue("id")
	if _, ok := s.data.Users[id]; !ok {
		apiError(w, errAPINotFound)
		return
	}
	apiJSON(w, 200, apiTokenList{s.userTokenViewsLocked(id)})
}
func (p *panel) apiAdminRevokeTokens(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	s := p.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := a.guardLocked(s, "admin")(); err != nil {
		apiError(w, err)
		return
	}
	user := r.PathValue("id")
	if _, ok := s.data.Users[user]; !ok {
		apiError(w, errAPINotFound)
		return
	}
	id := r.PathValue("tokenId")
	if err := s.revokeUserTokensLocked(user, id, id == "", a.userID); err != nil {
		apiError(w, err)
		return
	}
	w.WriteHeader(204)
}

func adminAPIRoutes() []apiRoute {
	return append([]apiRoute{
		{"GET", "/api/v1/admin/users", "admin", "admin-api-users", nil, reflect.TypeFor[apiUserList](), 200, (*panel).apiAdminUsers},
		{"POST", "/api/v1/admin/users", "admin", "admin-api-user-create", reflect.TypeFor[apiUserCreateRequest](), reflect.TypeFor[apiMeView](), 201, (*panel).apiAdminUserCreate},
		{"GET", "/api/v1/admin/users/{id}", "admin", "admin-api-user", nil, reflect.TypeFor[apiMeView](), 200, (*panel).apiAdminUser},
		{"PATCH", "/api/v1/admin/users/{id}", "admin", "admin-api-user-update", reflect.TypeFor[apiUserUpdateRequest](), reflect.TypeFor[apiMeView](), 200, (*panel).apiAdminUserUpdate},
		{"GET", "/api/v1/admin/users/{id}/tokens", "admin", "admin-api-user-tokens", nil, reflect.TypeFor[apiTokenList](), 200, (*panel).apiAdminUserTokens},
		{"DELETE", "/api/v1/admin/users/{id}/tokens", "admin", "admin-api-user-tokens-revoke", nil, nil, 204, (*panel).apiAdminRevokeTokens},
		{"DELETE", "/api/v1/admin/users/{id}/tokens/{tokenId}", "admin", "admin-api-user-token-revoke", nil, nil, 204, (*panel).apiAdminRevokeTokens},
	}, adminResourceRoutes()...)
}
