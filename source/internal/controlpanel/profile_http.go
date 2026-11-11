package controlpanel

import (
	"errors"
	"net/http"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

// Profile forms have no target ID. Reject extra fields instead of silently
// accepting attempts to change account or project authorization metadata.
func (p *panel) profileMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	allowed := map[string]bool{"csrf": true}
	if r.URL.Path == "/profile/save" {
		allowed["displayName"] = true
		allowed["email"] = true
		allowed["settings_version"] = true
	} else if r.URL.Path == "/profile/language" {
		allowed["language"] = true
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
	if r.URL.Path == "/profile/language" {
		if err := p.store.UpdateLanguage(session.userID, r.PostForm.Get("language")); err != nil {
			if errors.Is(err, ErrForbidden) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			p.show(w, r, session, "profile", "", http.StatusBadRequest, "profile.error_language", "")
			return
		}
		p.setLanguageCookie(w, r, p.store.UserLanguage(session.userID))
		p.redirectNotice(w, r, "/profile", "language-saved")
		return
	}
	var err error
	message := "profile.error_details"
	if r.URL.Path == "/profile/save" {
		err = p.store.UpdateProfile(session.userID, r.PostForm.Get("displayName"), r.PostForm.Get("email"), r.PostForm["settings_version"]...)
	} else {
		if !p.allowAttempt() {
			http.Error(w, i18n.T(p.requestLanguage(r, session.userID), "error.rate_limited"), http.StatusTooManyRequests)
			return
		}
		err = p.store.ChangeOwnPassword(session.userID, r.PostForm.Get("currentPassword"), r.PostForm.Get("newPassword"), r.PostForm.Get("confirmPassword"))
		message = "profile.error_password"
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
