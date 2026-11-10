package controlpanel

import (
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
)

type apiProjectsList struct {
	Items []projectDTO `json:"items"`
}
type apiMembersList struct {
	Items []memberDTO `json:"items"`
}
type apiProjectTokensList struct {
	Items []projectTokenDTO `json:"items"`
}
type apiScopeCatalog struct {
	Items []access.ScopeDetail `json:"items"`
}
type apiProjectCreateRequest struct {
	Name string `json:"name"`
}
type apiProjectPatchRequest struct {
	Mode       *string `json:"mode,omitempty"`
	Enabled    *bool   `json:"enabled,omitempty"`
	Version    *string `json:"version,omitempty"`
	Generation *string `json:"generation,omitempty"`
}
type apiMemberAddRequest struct {
	Username   string  `json:"username"`
	Role       string  `json:"role"`
	Generation *string `json:"generation,omitempty"`
}
type apiMemberPatchRequest struct {
	Role       string  `json:"role"`
	Generation *string `json:"generation,omitempty"`
}
type apiProjectTokenRequest struct {
	Label                 string   `json:"label,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	MaxAutoBindings       int      `json:"max_auto_bindings,omitempty"`
	AutoLifetimeDays      int      `json:"auto_lifetime_days,omitempty"`
	UnlimitedAutoBindings bool     `json:"unlimited_auto_bindings,omitempty"`
	UnlimitedAutoLifetime bool     `json:"unlimited_auto_lifetime,omitempty"`
	Generation            *string  `json:"generation,omitempty"`
}
type apiProjectTokenResponse struct {
	Token string `json:"token"`
	projectTokenDTO
}
type apiProjectScopesRequest struct {
	Scopes     []string `json:"scopes"`
	Version    string   `json:"version"`
	Generation *string  `json:"generation,omitempty"`
}

func (a apiPrincipal) projectGuardLocked(s *Store, name string, generation *string) commitGuard {
	return func() error {
		if err := a.guardLocked(s, "write").check(); err != nil {
			return err
		}
		if generation != nil {
			p, ok := s.data.Projects[name]
			if !ok || p.ProjectGenerationID != *generation {
				return errProjectGenerationMismatch
			}
		}
		return nil
	}
}
func (p *panel) apiProjectView(actor, name string) (projectDTO, error) {
	for _, v := range p.store.projectProjectionsFor(actor) {
		if v.Name == name {
			return v, nil
		}
	}
	return projectDTO{}, errAPINotFound
}
func (p *panel) apiProjects(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	apiJSON(w, 200, apiProjectsList{p.store.projectProjectionsFor(a.userID)})
}
func (p *panel) apiProject(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	v, err := p.apiProjectView(a.userID, r.PathValue("project"))
	if err != nil {
		apiError(w, err)
		return
	}
	apiJSON(w, 200, v)
}
func (p *panel) apiProjectCreate(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiProjectCreateRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	if err := p.authorizeMutation(a.userID, "/projects/create", ""); err != nil {
		apiError(w, err)
		return
	}
	if err := p.store.saveProjectForGuarded(a.userID, req.Name, "token", true, true, false, a.guardLocked(p.store, "write")); err != nil {
		p.apiProjectMutationError(w, err, req.Name, true)
		return
	}
	p.store.apiAuditAfter("POST /api/v1/projects", a.userID, req.Name, "")
	v, err := p.apiProjectView(a.userID, req.Name)
	if err != nil {
		apiError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+req.Name)
	apiJSON(w, 201, v)
}
func (p *panel) apiProjectPatch(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiProjectPatchRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	name := r.PathValue("project")
	if err := p.authorizeMutation(a.userID, "/projects/save", name); err != nil {
		apiError(w, err)
		return
	}
	s := p.store
	s.mu.Lock()
	current := s.data.Projects[name]
	mode, enabled := current.Mode, current.Enabled
	if req.Mode != nil {
		mode = *req.Mode
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	err := s.saveProjectForLocked(a.userID, name, mode, enabled, false, false, a.projectGuardLocked(s, name, req.Generation), apiVersion(req.Version)...)
	s.mu.Unlock()
	if err != nil {
		p.apiProjectMutationError(w, err, name, false)
		return
	}

	p.store.apiAuditAfter("PATCH /api/v1/projects/{project}", a.userID, name, "")
	v, err := p.apiProjectView(a.userID, name)
	if err != nil {
		apiError(w, err)
		return
	}
	apiJSON(w, 200, v)
}
func (p *panel) apiProjectDelete(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	name := r.PathValue("project")
	query := r.URL.Query()
	if len(query["confirm"]) != 1 || len(query["generation"]) != 1 || query.Get("generation") == "" || len(query) != 2 {
		apiError(w, ErrInvalid)
		return
	}
	if err := p.authorizeMutation(a.userID, "/projects/save", name); err != nil {
		apiError(w, err)
		return
	}
	if err := p.deleteProjectAs(r.Context(), a.userID, name, query.Get("confirm"), query.Get("generation"), a.guardLocked(p.store, "write")); err != nil {
		apiError(w, err)
		return
	}
	p.store.apiAuditAfter("DELETE /api/v1/projects/{project}", a.userID, name, "")
	w.WriteHeader(204)
}
func (p *panel) apiMembers(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	items, err := p.store.memberProjectionsFor(a.userID, r.PathValue("project"))
	if err != nil {
		apiError(w, err)
		return
	}
	apiJSON(w, 200, apiMembersList{items})
}
func (p *panel) apiMemberAdd(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiMemberAddRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	p.apiMemberChange(w, r, a, req.Username, req.Role, "add", req.Generation)
}
func (p *panel) apiMemberPatch(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiMemberPatchRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	p.apiMemberChange(w, r, a, r.PathValue("username"), req.Role, "update", req.Generation)
}
func (p *panel) apiMemberDelete(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	g, err := apiQueryGeneration(r)
	if err != nil {
		apiError(w, err)
		return
	}
	p.apiMemberChange(w, r, a, r.PathValue("username"), "", "remove", g)
}
func apiQueryGeneration(r *http.Request) (*string, error) {
	q := r.URL.Query()
	if len(q) == 0 {
		return nil, nil
	}
	if len(q) != 1 || len(q["generation"]) != 1 {
		return nil, ErrInvalid
	}
	g := q.Get("generation")
	return &g, nil
}
func (p *panel) apiMemberChange(w http.ResponseWriter, r *http.Request, a apiPrincipal, user, role, action string, generation *string) {
	name := r.PathValue("project")
	if err := p.authorizeMutation(a.userID, "/projects/members", name); err != nil {
		apiError(w, err)
		return
	}
	if _, err := p.store.changeProjectMemberGuarded(a.userID, name, user, role, action, a.projectGuardLocked(p.store, name, generation)); err != nil {
		apiError(w, err)
		return
	}
	template := "POST /api/v1/projects/{project}/members"
	if action == "update" {
		template = "PATCH /api/v1/projects/{project}/members/{username}"
	}
	if action == "remove" {
		template = "DELETE /api/v1/projects/{project}/members/{username}"
	}
	p.store.apiAuditAfter(template, a.userID, user, role)
	if action == "remove" {
		w.WriteHeader(204)
		return
	}
	u, _ := p.store.User(user)
	status := 200
	if action == "add" {
		w.Header().Set("Location", "/api/v1/projects/"+name+"/members/"+user)
		status = 201
	}
	apiJSON(w, status, memberDTO{ID: user, Role: role, Enabled: u.Enabled})
}
func (p *panel) apiProjectTokens(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	items, err := p.store.projectTokenProjectionsFor(a.userID, r.PathValue("project"))
	if err != nil {
		apiError(w, err)
		return
	}
	apiJSON(w, 200, apiProjectTokensList{items})
}
func (p *panel) apiProjectTokenIssue(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiProjectTokenRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	name := r.PathValue("project")
	if err := p.authorizeMutation(a.userID, "/tokens/project", name); err != nil {
		apiError(w, err)
		return
	}
	raw, metadata, err := p.store.issueProjectTokenGuarded(a.userID, name, req.Label, ProjectTokenOptions{Scopes: req.Scopes, MaxAutoBindings: req.MaxAutoBindings, AutoLifetimeDays: req.AutoLifetimeDays, UnlimitedAutoBindings: req.UnlimitedAutoBindings, UnlimitedAutoLifetime: req.UnlimitedAutoLifetime}, a.projectGuardLocked(p.store, name, req.Generation))
	if err != nil {
		apiError(w, err)
		return
	}
	p.store.apiAuditAfter("POST /api/v1/projects/{project}/tokens", a.userID, name, "")
	w.Header().Set("Location", "/api/v1/projects/"+name+"/tokens/"+metadata.ID)
	apiJSON(w, 201, apiProjectTokenResponse{raw, metadata})
}
func (p *panel) apiProjectTokenRevoke(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	name := r.PathValue("project")
	g, err := apiQueryGeneration(r)
	if err != nil {
		apiError(w, err)
		return
	}
	if err = p.authorizeMutation(a.userID, "/tokens/revoke", name); err != nil {
		apiError(w, err)
		return
	}
	if err = p.store.revokeTokenForGuarded(a.userID, name, r.PathValue("id"), a.projectGuardLocked(p.store, name, g)); err != nil {
		apiError(w, err)
		return
	}
	p.store.apiAuditAfter("DELETE /api/v1/projects/{project}/tokens/{id}", a.userID, name, "")
	w.WriteHeader(204)
}
func (p *panel) apiProjectTokenScopes(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiProjectScopesRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	name := r.PathValue("project")
	if err := p.authorizeMutation(a.userID, "/tokens/scopes", name); err != nil {
		apiError(w, err)
		return
	}
	if _, err := p.store.editTokenScopesForGuarded(a.userID, name, r.PathValue("id"), req.Scopes, []string{req.Version}, a.projectGuardLocked(p.store, name, req.Generation)); err != nil {
		apiError(w, err)
		return
	}
	p.store.apiAuditAfter("PUT /api/v1/projects/{project}/tokens/{id}/scopes", a.userID, name, "")
	items, err := p.store.projectTokenProjectionsFor(a.userID, name)
	if err != nil {
		apiError(w, err)
		return
	}
	for _, v := range items {
		if v.ID == r.PathValue("id") {
			apiJSON(w, 200, v)
			return
		}
	}
	apiError(w, errAPINotFound)
}
func (p *panel) apiTokenCatalog(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	apiJSON(w, 200, apiScopeCatalog{access.ScopeCatalog()})
}
func (p *panel) apiProjectMutationError(w http.ResponseWriter, err error, name string, create bool) {
	if errors.Is(err, ErrInvalid) && create && projectName.MatchString(name) {
		p.store.mu.RLock()
		_, exists := p.store.data.Projects[name]
		full := len(p.store.data.Projects) >= 128
		p.store.mu.RUnlock()
		if exists {
			apiJSON(w, 409, apiErrorView{"already_exists", "Project already exists."})
			return
		}
		if full {
			apiJSON(w, 409, apiErrorView{"project_cap_reached", "Project capacity reached."})
			return
		}
	}
	apiError(w, err)
}
