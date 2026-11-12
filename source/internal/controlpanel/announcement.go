package controlpanel

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/http"
	"net/url"
	"time"
)

// AnnouncementResult reports queue acceptance, never model receipt. Error is a machine code; the panel translates it.
type AnnouncementResult struct {
	Project, SessionID, Name, Error string
	Accepted                        bool
}

var announcementErrors = map[string]string{"project-deleting": "admin.announce.error_project_deleting", "sessions-unavailable": "admin.announce.error_sessions_unavailable", "announcement-unavailable": "admin.announce.error_unavailable", "not-ready": "admin.announce.error_not_ready", "queue-full": "admin.announce.error_queue_full", "suspended": "admin.announce.error_suspended", "not-accepted": "admin.announce.error_not_accepted"}

// ErrorMessage is the failure in the page's language; an unknown code reads as not accepted.
func (r AnnouncementResult) ErrorMessage() i18n.Message {
	if key, ok := announcementErrors[r.Error]; ok {
		return i18n.M(key)
	}
	return i18n.M("admin.announce.error_not_accepted")
}

type announcementSubmission struct {
	csrf               string
	expires            time.Time
	consumed, complete bool
	results            []AnnouncementResult
	message            string
}

func (p *panel) announcementMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || user.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	for key, values := range r.PostForm {
		if (key != "csrf" && key != "project" && key != "body" && key != "announcement_token") || len(values) != 1 {
			http.Error(w, "invalid form", 400)
			return
		}
	}
	body, project := r.PostForm.Get("body"), r.PostForm.Get("project")
	if err := protocol.ValidateAnnouncement(body, "", ""); err != nil {
		p.show(w, r, session, "admin-settings", "", 400, "admin.announce.error_body", "")
		return
	}
	if project != "" {
		found := false
		for _, pr := range p.store.ProjectsFor(user.ID) {
			if pr.Name == project && !pr.Deleting {
				found = true
			}
		}
		if !found {
			http.Error(w, "invalid project", 400)
			return
		}
	}
	backend, ok := p.backend.(interface {
		Announce(context.Context, string, string, string) ([]AnnouncementResult, error)
	})
	if !ok {
		http.Error(w, "announcement unavailable", http.StatusServiceUnavailable)
		return
	}
	id := r.PostForm.Get("announcement_token")
	p.mu.Lock()
	p.expireAnnouncements(time.Now())
	submission, exists := p.announcements[id]
	if !exists || submission.csrf != session.csrf {
		p.mu.Unlock()
		http.Error(w, "invalid or expired announcement token", http.StatusForbidden)
		return
	}
	if submission.consumed {
		p.mu.Unlock()
		p.redirectAnnouncement(w, r, id, true)
		return
	}
	// Claim before invoking the backend, including while another POST is in flight.
	submission.consumed = true
	p.mu.Unlock()
	results, err := backend.Announce(r.Context(), user.ID, project, body)
	if err != nil {
		message := "admin.announce.error_failed"
		if errors.Is(err, ErrForbidden) {
			message = "admin.announce.error_forbidden"
		}
		p.mu.Lock()
		submission.complete = true
		submission.message = message
		p.mu.Unlock()
		p.redirectAnnouncement(w, r, id, false)
		return
	}
	accepted := 0
	for _, result := range results {
		if result.Accepted {
			accepted++
		}
	}
	// No body, session names, credentials, URLs or downstream errors in this log.
	serverdiag.Emit(serverdiag.Event{Type: "admin-announcement", Targets: len(results), Accepted: accepted, Failed: len(results) - accepted})
	p.mu.Lock()
	submission.results = append([]AnnouncementResult(nil), results...)
	submission.complete = true
	p.mu.Unlock()
	p.redirectAnnouncement(w, r, id, false)
}

// Both the one-use form token and its result expire in memory. The receipt ID
// alone grants no access: POST and GET require the same current admin login.
func (p *panel) issueAnnouncementToken(session loginSession) string {
	id := rand.Text()
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.announcements == nil {
		p.announcements = map[string]*announcementSubmission{}
	}
	p.expireAnnouncements(now)
	if len(p.announcements) >= 256 {
		oldest := ""
		for key, entry := range p.announcements {
			if oldest == "" || entry.expires.Before(p.announcements[oldest].expires) {
				oldest = key
			}
		}
		delete(p.announcements, oldest) // Eviction rejects reuse, never re-enqueues.
	}
	p.announcements[id] = &announcementSubmission{csrf: session.csrf, expires: now.Add(5 * time.Minute)}
	return id
}

// p.mu must be held.
func (p *panel) expireAnnouncements(now time.Time) {
	for id, entry := range p.announcements {
		if !now.Before(entry.expires) {
			delete(p.announcements, id)
		}
	}
}
func (p *panel) redirectAnnouncement(w http.ResponseWriter, r *http.Request, id string, duplicate bool) {
	target := "/admin/announcement/result?id=" + url.QueryEscape(id)
	if duplicate {
		target += "&duplicate=true"
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target, http.StatusSeeOther)
}
func (p *panel) announcementResult(w http.ResponseWriter, r *http.Request, session loginSession) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || user.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ids := r.URL.Query()["id"]
	if len(ids) != 1 || ids[0] == "" {
		http.Error(w, "invalid result", 400)
		return
	}
	p.mu.Lock()
	p.expireAnnouncements(time.Now())
	entry, exists := p.announcements[ids[0]]
	if !exists || entry.csrf != session.csrf || !entry.consumed {
		p.mu.Unlock()
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	result := *entry
	p.mu.Unlock()
	if r.URL.Query().Get("duplicate") == "true" {
		result.message = "admin.announce.duplicate"
	}
	w.Header().Set("Referrer-Policy", "same-origin")
	p.show(w, r, session, "admin-settings", "", 200, "", "", &result)
}
