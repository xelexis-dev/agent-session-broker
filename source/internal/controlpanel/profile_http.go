package controlpanel

import (
	"errors"
	"net/http"
)

// Profile forms have no target ID. Reject extra fields instead of silently
// accepting attempts to change account or project authorization metadata.
func (p *panel) profileMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	allowed := map[string]bool{"csrf": true}
	if r.URL.Path == "/profile/save" {
		allowed["displayName"] = true
		allowed["email"] = true
		allowed["settings_version"] = true
	} else {
		allowed["currentPassword"] = true
		allowed["newPassword"] = true
		allowed["confirmPassword"] = true
	}
	if r.URL.RawQuery != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	for field, values := range r.PostForm {
		if field == "settings_version" && allowed[field] {
			continue
		}
		if !allowed[field] || len(values) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	var err error
	message := "표시 이름은 100자 이하로, 이메일은 올바른 주소로 입력해 주세요. 저장 상태도 확인해 주세요."
	if r.URL.Path == "/profile/save" {
		err = p.store.UpdateProfile(session.userID, r.PostForm.Get("displayName"), r.PostForm.Get("email"), r.PostForm["settings_version"]...)
	} else {
		if !p.allowAttempt() {
			http.Error(w, "잠시 후 다시 시도해 주세요.", http.StatusTooManyRequests)
			return
		}
		err = p.store.ChangeOwnPassword(session.userID, r.PostForm.Get("currentPassword"), r.PostForm.Get("newPassword"), r.PostForm.Get("confirmPassword"))
		message = "현재 비밀번호와 새 비밀번호 확인을 확인해 주세요. 새 비밀번호는 12~1024바이트여야 하며 저장할 수 있어야 합니다."
	}
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if errors.Is(err, errSettingsConflict) {
			p.show(w, r, session, "profile", "", http.StatusConflict, settingsConflictMessage, "")
			return
		}
		p.show(w, r, session, "profile", "", 400, message, "")
		return
	}
	if r.URL.Path == "/profile/password" {
		// Revision invalidates every prior login; promptly remove this cookie too.
		if cookie, e := r.Cookie("asb_admin"); e == nil {
			p.mu.Lock()
			delete(p.sessions, digest(cookie.Value))
			p.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "asb_admin", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: p.secure(r), SameSite: http.SameSiteStrictMode})
		p.redirectNotice(w, r, "/login", "password-saved")
		return
	}
	p.redirectNotice(w, r, "/profile", "profile-saved")
}
