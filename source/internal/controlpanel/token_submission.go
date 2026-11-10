package controlpanel

import (
	"crypto/rand"
	"time"
)

type tokenSubmission struct {
	csrf, project string
	expires       time.Time
}

// A rendered form identifies one issuance, independently of its label or tabs.
// Claim it before writing the token so concurrent/replayed POSTs cannot issue twice.
func (p *panel) issueTokenSubmission(session loginSession, project string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.tokenSubmissions == nil {
		p.tokenSubmissions = map[string]tokenSubmission{}
	}
	for id, entry := range p.tokenSubmissions {
		if !now.Before(entry.expires) {
			delete(p.tokenSubmissions, id)
		}
	}
	if len(p.tokenSubmissions) >= 256 {
		oldest := ""
		for id, entry := range p.tokenSubmissions {
			if oldest == "" || entry.expires.Before(p.tokenSubmissions[oldest].expires) {
				oldest = id
			}
		}
		delete(p.tokenSubmissions, oldest)
	}
	id := rand.Text()
	p.tokenSubmissions[id] = tokenSubmission{csrf: session.csrf, project: project, expires: now.Add(10 * time.Minute)}
	return id
}
func (p *panel) takeTokenSubmission(session loginSession, project, id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.tokenSubmissions[id]
	if !ok || entry.csrf != session.csrf || entry.project != project {
		return false
	}
	delete(p.tokenSubmissions, id)
	return time.Now().Before(entry.expires)
}
