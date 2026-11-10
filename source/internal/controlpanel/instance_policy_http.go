package controlpanel

import (
	"errors"
	"net/http"
)

// Keep the old mutation URL explicit so stale admin forms cannot change policy.
func (p *panel) instanceSettingsMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || user.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	http.Error(w, "token authentication is required; instance policy setting removed", http.StatusGone)
}

// instanceHTTPPolicyMutate accepts only the checkbox and CSRF fields so a stale
// per-project form cannot steer the instance-wide decision.
func (p *panel) instanceHTTPPolicyMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	for key, values := range r.PostForm {
		if key == "settings_version" {
			continue
		}
		if (key != "csrf" && key != "allowHTTP") || len(values) != 1 {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
	}
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || user.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := checkSettingsVersion(r.PostForm["settings_version"], policySettingsVersion("http-policy", p.store.InstanceHTTPPolicy())); err != nil {
		p.show(w, r, session, "admin-settings", "", http.StatusConflict, settingsConflictMessage, "")
		return
	}
	allow := false // unchecked HTML checkboxes are omitted.
	if values, exists := r.PostForm["allowHTTP"]; exists {
		if values[0] != "true" && values[0] != "false" {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		allow = values[0] == "true"
	}
	if err := p.store.SetInstanceHTTPPolicy(session.userID, allow, r.PostForm["settings_version"]...); err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if errors.Is(err, errSettingsConflict) {
			p.show(w, r, session, "admin-settings", "", http.StatusConflict, settingsConflictMessage, "")
			return
		}
		p.show(w, r, session, "admin-settings", "", http.StatusBadRequest, "HTTP 연결 정책을 저장하지 못했습니다.", "")
		return
	}
	p.redirectNotice(w, r, "/admin/settings", "instance-http-policy-saved")
}
