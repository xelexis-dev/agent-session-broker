package controlpanel

import (
	"crypto/rand"
	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
	"net/http"
	"time"
)

type pendingNotice struct {
	code, target string
	reportID     string
	expires      time.Time
}

const tokenScopeUpgradeMessage = "error.token_scope_upgrade"
const tokenScopeInvalidMessage = "error.token_scope_invalid"

const tokenCapMessage = "error.token_cap"

// noticeMessages maps a notice code to its catalog key; the text is chosen in the reader's language at render time.
var noticeMessages = noticeKeys("api-token-revoked", "language-saved", "bug-report-deleted", "bug-report-delete-incomplete", "bug-report-resolved",
	"bug-report-ignored", "bug-report-open", "token-scopes-saved", "session-grant-released", "session-suspended",
	"session-unsuspended", "user-saved-relogin", "project-created", "project-saved", "instance-http-policy-saved",
	"members-unchanged", "members-saved", "token-revoked", "session-detached", "user-created", "user-saved",
	"profile-saved", "password-saved", "setup-saved", "project-deleted", "device-approved", "device-denied")

func noticeKeys(codes ...string) map[string]string {
	keys := map[string]string{}
	for _, code := range codes {
		keys[code] = "notice." + code
	}
	return keys
}

// A random, single-use cookie refers to a fixed server-side message. It conveys
// no authorization and contains neither form values nor credentials.
func (p *panel) redirectNotice(w http.ResponseWriter, r *http.Request, target, code string, reportID ...string) {
	if noticeMessages[code] != "" {
		now := time.Now()
		key := rand.Text()
		p.mu.Lock()
		if p.notices == nil {
			p.notices = map[string]pendingNotice{}
		}
		for id, n := range p.notices {
			if !now.Before(n.expires) {
				delete(p.notices, id)
			}
		}
		// Bound transient storage independently of user actions or abandoned tabs.
		if len(p.notices) >= 256 {
			for id := range p.notices {
				delete(p.notices, id)
				break
			}
		}
		notice := pendingNotice{code: code, target: target, expires: now.Add(2 * time.Minute)}
		if code == "bug-report-deleted" && len(reportID) == 1 {
			notice.reportID = reportID[0]
		}
		p.notices[key] = notice
		p.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "asb_notice", Value: key, Path: "/", MaxAge: 120, HttpOnly: true, Secure: p.secure(r), SameSite: http.SameSiteStrictMode})
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (p *panel) takeNotice(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("asb_notice")
	if err != nil {
		return ""
	}
	p.mu.Lock()
	n, ok := p.notices[cookie.Value]
	if ok && time.Now().Before(n.expires) && (r.Method != http.MethodGet || r.URL.RequestURI() != n.target) {
		p.mu.Unlock()
		return ""
	}
	delete(p.notices, cookie.Value)
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "asb_notice", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: p.secure(r), SameSite: http.SameSiteStrictMode})
	if !ok || !time.Now().Before(n.expires) {
		return ""
	}
	if n.code == "bug-report-deleted" && n.reportID != "" {
		return i18n.T(p.requestLanguage(r, ""), "notice.bug-report-deleted-id", "id", n.reportID)
	}
	return noticeMessages[n.code]
}
