package controlpanel

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"time"
)

//go:embed admin-openapi.json
var adminOpenAPI []byte

type apiLoginRequest struct {
	Username  string   `json:"username"`
	Password  string   `json:"password"`
	Label     string   `json:"label,omitempty"`
	ExpiresIn int64    `json:"expires_in,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}
type apiLoginResponse struct {
	Token string `json:"token"`
	UserTokenView
}
type apiMeView struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	Version     string `json:"version"`
}

func apiUserView(u User) apiMeView {
	return apiMeView{u.ID, u.Username, u.DisplayName, u.Email, u.Role, u.Enabled, profileSettingsVersion(u)}
}

type apiProfileRequest struct {
	DisplayName *string `json:"display_name,omitempty"`
	Email       *string `json:"email,omitempty"`
	Version     *string `json:"version,omitempty"`
}
type apiPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	Password        string `json:"password"`
}
type apiTokenList struct {
	Items []UserTokenView `json:"items"`
}
type apiErrorView struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type apiRoute struct {
	Method, Path, Scope, Parity string
	Request, Response           reflect.Type
	Status                      int
	Handler                     func(*panel, http.ResponseWriter, *http.Request, apiPrincipal)
}

var apiRoutes = append([]apiRoute{
	{"POST", "/api/v1/login", "", "login", reflect.TypeFor[apiLoginRequest](), reflect.TypeFor[apiLoginResponse](), 201, (*panel).apiLogin},
	{"POST", "/api/v1/logout", "", "logout", nil, nil, 204, (*panel).apiLogout},
	{"GET", "/api/v1/me", "read", "profile", nil, reflect.TypeFor[apiMeView](), 200, (*panel).apiMe},
	{"PATCH", "/api/v1/me", "write", "profile", reflect.TypeFor[apiProfileRequest](), reflect.TypeFor[apiMeView](), 200, (*panel).apiProfile},
	{"POST", "/api/v1/me/password", "write", "profile-password", reflect.TypeFor[apiPasswordRequest](), nil, 204, (*panel).apiPassword},
	{"GET", "/api/v1/me/tokens", "read", "own-tokens", nil, reflect.TypeFor[apiTokenList](), 200, (*panel).apiTokens},
	{"DELETE", "/api/v1/me/tokens/{id}", "write", "own-tokens", nil, nil, 204, (*panel).apiRevoke},
	{"GET", "/api/v1/openapi.json", "", "openapi", nil, nil, 200, (*panel).apiSpec},
	{"GET", "/api/v1/projects", "read", "project-list", nil, reflect.TypeFor[apiProjectsList](), 200, (*panel).apiProjects},
	{"POST", "/api/v1/projects", "write", "project-create", reflect.TypeFor[apiProjectCreateRequest](), reflect.TypeFor[projectDTO](), 201, (*panel).apiProjectCreate},
	{"GET", "/api/v1/projects/{project}", "read", "project-get", nil, reflect.TypeFor[projectDTO](), 200, (*panel).apiProject},
	{"PATCH", "/api/v1/projects/{project}", "write", "project-patch", reflect.TypeFor[apiProjectPatchRequest](), reflect.TypeFor[projectDTO](), 200, (*panel).apiProjectPatch},
	{"DELETE", "/api/v1/projects/{project}", "write", "project-delete", nil, nil, 204, (*panel).apiProjectDelete},
	{"GET", "/api/v1/projects/{project}/members", "read", "member-list", nil, reflect.TypeFor[apiMembersList](), 200, (*panel).apiMembers},
	{"POST", "/api/v1/projects/{project}/members", "write", "member-add", reflect.TypeFor[apiMemberAddRequest](), reflect.TypeFor[memberDTO](), 201, (*panel).apiMemberAdd},
	{"PATCH", "/api/v1/projects/{project}/members/{username}", "write", "member-update", reflect.TypeFor[apiMemberPatchRequest](), reflect.TypeFor[memberDTO](), 200, (*panel).apiMemberPatch},
	{"DELETE", "/api/v1/projects/{project}/members/{username}", "write", "member-remove", nil, nil, 204, (*panel).apiMemberDelete},
	{"GET", "/api/v1/projects/{project}/tokens", "read", "project-token-list", nil, reflect.TypeFor[apiProjectTokensList](), 200, (*panel).apiProjectTokens},
	{"POST", "/api/v1/projects/{project}/tokens", "write", "project-token-issue", reflect.TypeFor[apiProjectTokenRequest](), reflect.TypeFor[apiProjectTokenResponse](), 201, (*panel).apiProjectTokenIssue},
	{"DELETE", "/api/v1/projects/{project}/tokens/{id}", "write", "project-token-revoke", nil, nil, 204, (*panel).apiProjectTokenRevoke},
	{"PUT", "/api/v1/projects/{project}/tokens/{id}/scopes", "write", "project-token-scopes", reflect.TypeFor[apiProjectScopesRequest](), reflect.TypeFor[projectTokenDTO](), 200, (*panel).apiProjectTokenScopes},
	{"GET", "/api/v1/token-scope-catalog", "read", "token-catalog", nil, reflect.TypeFor[apiScopeCatalog](), 200, (*panel).apiTokenCatalog},
}, adminAPIRoutes()...)

func apiJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
func apiError(w http.ResponseWriter, err error) {
	status, code, message := 400, "invalid_request", "Invalid request."
	switch {
	case errors.Is(err, errAPIUnauthorized):
		status, code, message = 401, "invalid_token", "A valid user bearer token is required."
		w.Header().Set("WWW-Authenticate", `Bearer realm="asb-admin", error="invalid_token"`)
	case errors.Is(err, errAPICredentials):
		status, code, message = 401, "invalid_credentials", "The credential is invalid."
	case errors.Is(err, errAPIScope):
		status, code, message = 403, "insufficient_scope", "The token does not grant the required scope."
	case errors.Is(err, errAPITokenCap):
		status, code, message = 409, "token_cap_reached", "User token capacity reached."
	case errors.Is(err, errAPINotFound):
		status, code, message = 404, "not_found", "Resource not found."
	case errors.Is(err, errAPIJSON):
		status, code, message = 400, "invalid_json", "A single valid JSON object is required."
	case errors.Is(err, errAPIUnknownField):
		status, code, message = 400, "unknown_field", "Unknown JSON field."
	case errors.Is(err, errAPIMedia):
		status, code, message = 415, "unsupported_media_type", "Content-Type must be application/json."
	case errors.Is(err, errAPILarge):
		status, code, message = 413, "payload_too_large", "Request body exceeds 16 KiB."
	case errors.Is(err, errAPIDisabled):
		status, code, message = 403, "api_disabled", "Management API is disabled."
	case errors.Is(err, errHTTPNotAllowed):
		status, code, message = 403, "http_not_allowed", "The instance does not allow plaintext credentials."
	case errors.Is(err, ErrForbidden):
		status, code, message = 403, "forbidden", "Permission denied."
	case errors.Is(err, errAPIBusy):
		status, code, message = 503, "busy", "Password verifier is busy."
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, errAPIRate):
		status, code, message = 429, "rate_limited", "Request budget exceeded."
		w.Header().Set("Retry-After", "60")
	case errors.Is(err, ErrStorage):
		status, code, message = 503, "unavailable", "Operation blocked; persistence is unavailable."
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, errProjectGenerationMismatch):
		status, code, message = 409, "generation_conflict", "Project generation changed."
	case errors.Is(err, ErrTokenCap):
		status, code, message = 409, "token_cap_reached", "Project token capacity reached."
	case errors.Is(err, errScopeUpgradeRequired):
		status, code, message = 409, "scope_issuance_disabled", "Scoped issuance is disabled."
	case errors.Is(err, errProjectDeletePending), errors.Is(err, errProjectDeleteUnavailable), errors.Is(err, errProjectDeleteInterrupted):
		status, code, message = 503, "deletion_pending", "Project deletion is pending."
	case errors.Is(err, errDeviceCodeAttempts):
		status, code, message = 429, "rate_limited", "Device code attempt budget exceeded."
		w.Header().Set("Retry-After", "60")
	case errors.Is(err, ErrNicknameTaken):
		status, code, message = 409, "nickname_taken", "Nickname is already reserved."
	case errors.Is(err, errDeviceGrant):
		status, code, message = 409, "already_decided", "Device approval is no longer pending."
	case errors.Is(err, errSettingsConflict):
		status, code, message = 409, "version_conflict", "Settings changed."
	}
	apiJSON(w, status, apiErrorView{code, message})
}

// decodeAPIJSON rejects duplicates at every nesting level before struct decoding.
func decodeAPIJSON(reader io.Reader, target any) error {
	raw, err := io.ReadAll(io.LimitReader(reader, 16*1024+1))
	if err != nil {
		return errAPIJSON
	}
	if len(raw) > 16*1024 {
		return errAPILarge
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	var value func() error
	value = func() error {
		tok, e := d.Token()
		if e != nil {
			return errAPIJSON
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				key, ok := k.(string)
				if e != nil || !ok || seen[key] {
					return errAPIJSON
				}
				seen[key] = true
				if value() != nil {
					return errAPIJSON
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return errAPIJSON
			}
		case '[':
			for d.More() {
				if value() != nil {
					return errAPIJSON
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return errAPIJSON
			}
		default:
			return errAPIJSON
		}
		return nil
	}
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw))[0] != '{' || value() != nil {
		return errAPIJSON
	}
	if _, e := d.Token(); e != io.EOF {
		return errAPIJSON
	}
	typ := reflect.TypeOf(target)
	if typ == nil || typ.Kind() != reflect.Pointer || typ.Elem().Kind() != reflect.Struct {
		return errAPIJSON
	}
	allowed := map[string]bool{}
	for i := 0; i < typ.Elem().NumField(); i++ {
		field := typ.Elem().Field(i)
		allowed[strings.Split(field.Tag.Get("json"), ",")[0]] = true
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return errAPIJSON
	}
	for key := range object {
		if !allowed[key] {
			return errAPIUnknownField
		}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if dec.Decode(target) != nil {
		return errAPIJSON
	}
	return nil
}
func readAPIBody(r *http.Request, target any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return errAPIMedia
	}
	return decodeAPIJSON(r.Body, target)
}
func (p *panel) apiMux() *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range apiRoutes {
		mux.HandleFunc(route.Method+" "+route.Path, func(w http.ResponseWriter, r *http.Request) {
			if route.Parity == "openapi" {
				route.Handler(p, w, r, apiPrincipal{})
				return
			}
			if !p.store.AdminAPIEnabled() {
				apiError(w, errAPIDisabled)
				return
			}
			if p.plaintext(r) {
				p.store.mu.RLock()
				allow := p.store.data.AllowHTTP
				p.store.mu.RUnlock()
				if !allow {
					apiError(w, errHTTPNotAllowed)
					return
				}
			}
			if route.Parity == "login" {
				route.Handler(p, w, r, apiPrincipal{})
				return
			}
			headers := r.Header.Values("Authorization")
			if len(headers) != 1 || !strings.HasPrefix(headers[0], "Bearer ") {
				apiError(w, errAPIUnauthorized)
				return
			}
			a, err := p.store.authenticateAPI(strings.TrimPrefix(headers[0], "Bearer "))
			if err != nil {
				apiError(w, err)
				return
			}
			a.plaintext = p.plaintext(r)
			p.store.mu.Lock()
			err = a.guardLocked(p.store, route.Scope)()
			if err == nil && route.Method != "GET" && !rateAllowed(p.store.api.writes, a.userID, p.store.now(), 60, time.Minute) {
				err = errAPIRate
			}
			p.store.mu.Unlock()
			if err != nil {
				apiError(w, err)
				return
			}
			route.Handler(p, w, r, a)
		})
	}
	return mux
}
func (p *panel) serveAPI(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	result := &apiStatusWriter{ResponseWriter: w, status: 200}
	w = result
	defer func() {
		if result.status >= 400 && r.Method != "GET" {
			p.store.mu.Lock()
			p.store.apiRefusalLocked("write_refused")
			p.store.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	// ServeMux computes 404/405 and Allow; substitute a JSON contract for its text errors.
	mux := p.apiHandler
	_, pattern := mux.Handler(r)
	if pattern == "" {
		methods := []string{}
		for _, route := range apiRoutes {
			trial := r.Clone(r.Context())
			trial.Method = route.Method
			if _, matched := mux.Handler(trial); matched != "" {
				methods = append(methods, route.Method)
			}
		}
		if len(methods) > 0 {
			w.Header().Set("Allow", strings.Join(methods, ", "))
			apiJSON(w, 405, apiErrorView{"method_not_allowed", "Method not allowed."})
		} else {
			apiJSON(w, 404, apiErrorView{"not_found", "Route not found."})
		}
		return
	}
	mux.ServeHTTP(w, r)
}
func (p *panel) apiSpec(w http.ResponseWriter, r *http.Request, _ apiPrincipal) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(adminOpenAPI)
}
func (p *panel) apiLogin(w http.ResponseWriter, r *http.Request, _ apiPrincipal) {
	var req apiLoginRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	raw, view, err := p.store.issueUserToken(req.Username, req.Password, req.Label, req.ExpiresIn, req.Scopes, "login", p.transport(r).peer, p.plaintext(r), 0)
	if err != nil {
		apiError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/me/tokens/"+view.ID)
	apiJSON(w, 201, apiLoginResponse{raw, view})
}
func (p *panel) apiLogout(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	p.apiRevokeID(w, a, a.tokenID, "")
}
func (p *panel) apiRevoke(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	p.apiRevokeID(w, a, r.PathValue("id"), "write")
}
func (p *panel) apiRevokeID(w http.ResponseWriter, a apiPrincipal, id, scope string) {
	s := p.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := a.guardLocked(s, scope)(); err != nil {
		apiError(w, err)
		return
	}
	if err := s.revokeUserTokensLocked(a.userID, id, false); err != nil {
		apiError(w, err)
		return
	}
	w.WriteHeader(204)
}
func (p *panel) apiMe(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	s := p.store
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := a.guardLocked(s, "read")(); err != nil {
		apiError(w, err)
		return
	}
	apiJSON(w, 200, apiUserView(s.data.Users[a.userID].User))
}
func (p *panel) apiTokens(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	s := p.store
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := a.guardLocked(s, "read")(); err != nil {
		apiError(w, err)
		return
	}
	apiJSON(w, 200, apiTokenList{s.userTokenViewsLocked(a.userID)})
}
func (p *panel) apiProfile(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiProfileRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	s := p.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := a.guardLocked(s, "write")(); err != nil {
		apiError(w, err)
		return
	}
	u := s.data.Users[a.userID]
	if err := checkSettingsVersion(apiVersion(req.Version), profileSettingsVersion(u.User)); err != nil {
		apiError(w, err)
		return
	}
	if req.DisplayName != nil {
		u.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Email != nil {
		u.Email = *req.Email
	}
	if !validProfile(u.DisplayName, u.Email) {
		apiError(w, ErrInvalid)
		return
	}
	candidate := s.data
	candidate.Users = maps.Clone(s.data.Users)
	candidate.Users[a.userID] = u
	if err := s.apiCommitLocked(candidate, apiAuditEvent{Action: "profile_update", Actor: a.userID, Result: "success"}); err != nil {
		apiError(w, err)
		return
	}
	apiJSON(w, 200, apiUserView(u.User))
}
func (p *panel) apiPassword(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiPasswordRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	if len(req.CurrentPassword) > 1024 || len(req.Password) < 12 || len(req.Password) > 1024 {
		apiError(w, ErrInvalid)
		return
	}
	s := p.store
	s.mu.Lock()
	if err := a.guardLocked(s, "write")(); err != nil {
		s.mu.Unlock()
		apiError(w, err)
		return
	}
	select {
	case s.hashSlot <- struct{}{}:
	default:
		s.mu.Unlock()
		apiError(w, errAPIBusy)
		return
	}
	defer func() { <-s.hashSlot }()
	if !apiReauthAvailable(s.api.reauth, a.tokenID, s.now()) {
		s.mu.Unlock()
		apiError(w, errAPIRate)
		return
	}
	u := s.data.Users[a.userID]
	s.mu.Unlock()
	verified := verifyAPISecret(u.Password, req.CurrentPassword)
	var verifier passwordVerifier
	var err error
	if verified {
		verifier, err = hashPassword(req.Password)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !verified {
		rateAllowed(s.api.reauth, a.tokenID, s.now(), 5, 15*time.Minute)
		s.apiRefusalLocked("reauth_failed")
		apiError(w, errAPICredentials)
		return
	}
	if err != nil {
		apiError(w, err)
		return
	}
	if err = a.guardLocked(s, "write")(); err != nil {
		apiError(w, err)
		return
	}
	// Preserve metadata changed during KDF; guard pins the credential revision.
	u = s.data.Users[a.userID]
	u.Password = verifier
	u.Revision++
	candidate := s.data
	candidate.Users = maps.Clone(s.data.Users)
	candidate.Users[a.userID] = u
	if a.userID == "admin" {
		candidate.Admin = &verifier
	}
	if err = s.apiCommitLocked(candidate, apiAuditEvent{Action: "password_change", Actor: a.userID, Result: "success"}); err != nil {
		_ = s.invalidateAPITokensLocked(a.userID)
		apiError(w, err)
		return
	}
	_ = s.invalidateAPITokensLocked(a.userID)
	w.WriteHeader(204)
}
func apiReauthAvailable(m map[string]apiRate, key string, now time.Time) bool {
	v := m[key]
	return !now.Before(v.until) || v.n < 5
}
func (s *Store) invalidateAPITokensLocked(user string) error {
	for id, t := range s.api.tokens {
		if t.User == user {
			delete(s.api.tokens, id)
			s.api.pendingDeletes[t.key()] = true
		}
	}
	return nil
}

func apiVersion(v *string) []string {
	if v == nil {
		return nil
	}
	return []string{*v}
}

type apiStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *apiStatusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
