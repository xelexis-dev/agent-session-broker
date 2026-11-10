package controlpanel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

func panelAPIFields(r *http.Request, fields ...string) bool {
	if r.URL.RawQuery != "" {
		return false
	}
	allowed := map[string]bool{"csrf": true}
	for _, field := range fields {
		allowed[field] = true
	}
	for field, values := range r.PostForm {
		if !allowed[field] || field != "scope" && len(values) != 1 {
			return false
		}
	}
	return true
}
func (p *panel) adminAPIPolicyMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || user.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if len(r.PostForm["settings_version"]) != 1 || !panelAPIFields(r, "settings_version", "enabled") || (r.PostForm.Get("enabled") != "true" && r.PostForm.Get("enabled") != "false") {
		http.Error(w, "invalid form", 400)
		return
	}
	err := p.store.SetAdminAPI(session.userID, r.PostForm.Get("enabled") == "true", r.PostForm["settings_version"]...)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", 403)
			return
		}
		status := 400
		message := "관리 API 설정을 저장하지 못했습니다."
		if errors.Is(err, errSettingsConflict) {
			status = 409
			message = settingsConflictMessage
		}
		if errors.Is(err, ErrStorage) {
			status = 503
		}
		p.show(w, r, session, "admin-settings", "", status, message, "")
		return
	}
	p.redirectNotice(w, r, "/admin/settings", "admin-api-saved")
}
func (p *panel) profileAPITokensMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	path := r.URL.Path
	if path == "/profile/api-tokens" {
		if !panelAPIFields(r, "label", "expires_days", "scope", "current_password") {
			http.Error(w, "invalid form", 400)
			return
		}
		if len(r.PostForm["scope"]) == 0 {
			p.profileAPITokenError(w, r, session, ErrInvalid)
			return
		}
		days := r.PostForm.Get("expires_days")
		if days == "" {
			days = "30"
		}
		if days != "1" && days != "7" && days != "30" && days != "90" {
			p.profileAPITokenError(w, r, session, ErrInvalid)
			return
		}
		n, _ := strconv.ParseInt(days, 10, 64)
		cookie, err := r.Cookie("asb_admin")
		if err != nil {
			http.Error(w, "forbidden", 403)
			return
		}
		raw, _, err := p.store.IssueOwnUserToken(session.userID, session.revision, r.PostForm.Get("label"), n*24*3600, r.PostForm["scope"], digest(cookie.Value), p.plaintext(r), p.allowAttempt)
		if err != nil {
			p.profileAPITokenError(w, r, session, err)
			return
		}
		p.redirectIssuedTokenKind(w, r, session, session.userID, raw, "user")
		return
	}
	fields := []string{}
	if path == "/profile/api-tokens/revoke" {
		fields = append(fields, "id")
	}
	if !panelAPIFields(r, fields...) {
		http.Error(w, "invalid form", 400)
		return
	}
	var err error
	if path == "/profile/api-tokens/revoke" {
		err = p.store.RevokeOwnUserToken(session.userID, r.PostForm.Get("id"))
	} else {
		err = p.store.RevokeOwnUserTokens(session.userID)
	}
	if err != nil {
		p.profileAPITokenError(w, r, session, err)
		return
	}
	p.redirectNotice(w, r, "/profile/api-tokens", "api-token-revoked")
}
func (p *panel) profileAPITokenError(w http.ResponseWriter, r *http.Request, session loginSession, err error) {
	status, code, message := 400, "invalid", "발급 요청을 확인해 주세요."
	switch {
	case errors.Is(err, errAPITokenCap):
		status, code, message = 409, "cap", "발급 한도에 도달했습니다. 사용하지 않는 토큰을 취소해 주세요."
	case errors.Is(err, errAPIRate):
		status, code, message = 429, "rate", "시도가 너무 많습니다. 잠시 후 다시 시도해 주세요."
	case errors.Is(err, errHTTPNotAllowed):
		status, code, message = 403, "http", "HTTP에서는 API 토큰을 발급하거나 표시할 수 없습니다."
	case errors.Is(err, errAPIBusy):
		status, code, message = 503, "busy", "처리 중입니다. 잠시 후 다시 시도해 주세요."
	case errors.Is(err, ErrForbidden):
		status, code, message = 403, "forbidden", "권한이 없습니다."
	case errors.Is(err, ErrStorage):
		status, code, message = 503, "storage", "저장하지 못했습니다. 취소 요청은 현재 서버에서 차단되며 다시 저장합니다."
	}
	// Never retain a password (or the form body) in the rendered page state.
	p.showAPITokenError(w, r, session, status, message, code)
}
func (p *panel) showAPITokenError(w http.ResponseWriter, r *http.Request, session loginSession, status int, message, code string) {
	request := r.Clone(context.WithValue(r.Context(), apiPanelErrorKey{}, code))
	request.PostForm = url.Values{}
	request.Form = url.Values{}
	view := "api-token-new"
	if r.URL.Path != "/profile/api-tokens" {
		view = "api-tokens"
	}
	p.show(w, request, session, view, "", status, message, "")
}
func (p *panel) adminUserAPITokensMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || user.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	fields := []string{"user"}
	all := r.URL.Path == "/admin/users/api-tokens/revoke-all"
	if !all {
		fields = append(fields, "id")
	}
	if !panelAPIFields(r, fields...) {
		http.Error(w, "invalid form", 400)
		return
	}
	target := r.PostForm.Get("user")
	err := p.store.RevokeUserTokensFor(session.userID, target, r.PostForm.Get("id"), all)
	if err != nil {
		status := 400
		if errors.Is(err, ErrForbidden) {
			status = 403
		}
		if errors.Is(err, ErrStorage) {
			status = 503
		}
		http.Error(w, "API token revocation failed", status)
		return
	}
	p.redirectNotice(w, r, "/admin/users/detail?id="+url.QueryEscape(target), "api-token-revoked")
}

type apiPanelErrorKey struct{}
