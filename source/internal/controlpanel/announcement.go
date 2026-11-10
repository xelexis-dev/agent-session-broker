package controlpanel

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"log"
	"net/http"
	"net/url"
	"time"
)

// AnnouncementResult reports queue acceptance, never model receipt.
type AnnouncementResult struct {
	Project, SessionID, Name, Error string
	Accepted                        bool
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
		p.show(w, r, session, "admin-settings", "", 400, "공지 내용을 입력하고 12000바이트 envelope 한도를 확인하세요.", "")
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
		message := "공지를 접수하지 못했습니다. 내용 크기와 대상 프로젝트를 확인하세요."
		if errors.Is(err, ErrForbidden) {
			message = "공지 권한을 다시 확인하세요."
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
	log.Printf("admin announcement actor=%q targets=%d accepted=%d failed=%d", user.ID, len(results), accepted, len(results)-accepted)
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
		result.message = "이미 접수됨. 같은 공지를 다시 접수하지 않았습니다."
	}
	if !result.complete {
		result.message += " 공지 접수 처리 중입니다."
	}
	w.Header().Set("Referrer-Policy", "same-origin")
	p.show(w, r, session, "admin-settings", "", 200, "", "", &result)
}
