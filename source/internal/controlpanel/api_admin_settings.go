package controlpanel

import (
	"net/http"
	"reflect"
	"time"
)

type apiPolicyChangeView struct {
	Actor string    `json:"actor"`
	At    time.Time `json:"at"`
}
type apiPolicyView struct {
	Enabled    bool                 `json:"enabled"`
	Version    string               `json:"version"`
	LastChange *apiPolicyChangeView `json:"last_change"`
}
type apiSettingsView struct {
	HTTPPolicy    apiPolicyView `json:"http_policy"`
	ScopeIssuance apiPolicyView `json:"scope_issuance"`
	Attachments   apiPolicyView `json:"attachments"`
	AdminAPI      bool          `json:"admin_api"`
}
type apiHTTPPolicyRequest struct {
	AllowHTTP       *bool   `json:"allow_http"`
	CurrentPassword string  `json:"current_password,omitempty"`
	Version         *string `json:"version,omitempty"`
}
type apiPolicyRequest struct {
	Enabled *bool   `json:"enabled"`
	Version *string `json:"version,omitempty"`
}

func (p *panel) apiSettings(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	s := p.store
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := a.guardLocked(s, "admin")(); err != nil {
		apiError(w, err)
		return
	}
	view := apiSettingsView{HTTPPolicy: apiPolicyView{Enabled: s.data.AllowHTTP, Version: policySettingsVersion("http-policy", s.data.AllowHTTP)}, ScopeIssuance: apiPolicyView{Enabled: bool(s.data.ScopeIssuanceReady), Version: policySettingsVersion("scope-issuance", bool(s.data.ScopeIssuanceReady))}, Attachments: apiPolicyView{Enabled: s.attachmentEnabled, Version: policySettingsVersion("attachments", s.attachmentEnabled)}, AdminAPI: s.api.enabled}
	if changes := s.data.ScopeIssuanceChanges; len(changes) > 0 {
		last := changes[len(changes)-1]
		view.ScopeIssuance.LastChange = &apiPolicyChangeView{last.Actor, last.At}
	}
	if last := s.attachmentLastChange; last != nil {
		view.Attachments.LastChange = &apiPolicyChangeView{last.Actor, last.At}
	}
	apiJSON(w, 200, view)
}
func (p *panel) apiHTTPPolicy(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiHTTPPolicyRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	if req.AllowHTTP == nil {
		apiError(w, ErrInvalid)
		return
	}
	release, verified, err := p.store.apiPasswordProof(a, req.CurrentPassword, "admin", false)
	if err != nil {
		apiError(w, err)
		return
	}
	defer release()
	s := p.store
	guard := func() error {
		if err := a.guardLocked(s, "admin")(); err != nil {
			return err
		}
		if !s.data.AllowHTTP && *req.AllowHTTP && !verified {
			return ErrForbidden
		}
		return nil
	}
	if err := s.setInstanceHTTPPolicyGuarded(a.userID, *req.AllowHTTP, guard, apiVersion(req.Version)...); err != nil {
		apiError(w, err)
		return
	}
	s.apiAuditAfter("PUT /api/v1/admin/settings/http-policy", a.userID, "http-policy", "")
	w.WriteHeader(204)
}
func (p *panel) apiScopePolicy(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiPolicyRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	if req.Enabled == nil {
		apiError(w, ErrInvalid)
		return
	}
	s := p.store
	if err := s.setScopeIssuanceReadyGuarded(a.userID, *req.Enabled, a.guardLocked(s, "admin"), apiVersion(req.Version)...); err != nil {
		apiError(w, err)
		return
	}
	s.apiAuditAfter("PUT /api/v1/admin/settings/scope-issuance", a.userID, "scope-issuance", "")
	w.WriteHeader(204)
}
func (p *panel) apiAttachmentsPolicy(w http.ResponseWriter, r *http.Request, a apiPrincipal) {
	var req apiPolicyRequest
	if err := readAPIBody(r, &req); err != nil {
		apiError(w, err)
		return
	}
	if req.Enabled == nil {
		apiError(w, ErrInvalid)
		return
	}
	s := p.store
	if err := s.setAttachmentPolicyGuarded(a.userID, *req.Enabled, a.guardLocked(s, "admin"), apiVersion(req.Version)...); err != nil {
		apiError(w, err)
		return
	}
	s.apiAuditAfter("PUT /api/v1/admin/settings/attachments", a.userID, "attachments", "")
	w.WriteHeader(204)
}
func adminSettingsRoutes() []apiRoute {
	return []apiRoute{
		{"GET", "/api/v1/admin/settings", "admin", "admin-api-settings", nil, reflect.TypeFor[apiSettingsView](), 200, (*panel).apiSettings},
		{"PUT", "/api/v1/admin/settings/http-policy", "admin", "admin-api-http-policy", reflect.TypeFor[apiHTTPPolicyRequest](), nil, 204, (*panel).apiHTTPPolicy},
		{"PUT", "/api/v1/admin/settings/scope-issuance", "admin", "admin-api-scope-policy", reflect.TypeFor[apiPolicyRequest](), nil, 204, (*panel).apiScopePolicy},
		{"PUT", "/api/v1/admin/settings/attachments", "admin", "admin-api-attachment-policy", reflect.TypeFor[apiPolicyRequest](), nil, 204, (*panel).apiAttachmentsPolicy},
	}
}
