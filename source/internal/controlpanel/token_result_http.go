package controlpanel

import (
	"crypto/rand"
	"net/http"
	"net/url"
	"time"
)

type issuedTokenResult struct {
	raw, csrf, project string
	kind               string
	expires            time.Time
}

// Keep the one-time secret out of the redirect URL, cookies and durable storage.
// Each receipt belongs to one login and project, so separate tabs cannot consume
// one another's result. Expired entries are swept on access; storage is bounded.
func (p *panel) redirectIssuedToken(w http.ResponseWriter, r *http.Request, session loginSession, project, raw string) {
	p.redirectIssuedTokenKind(w, r, session, project, raw, "project")
}

func (p *panel) redirectIssuedTokenKind(w http.ResponseWriter, r *http.Request, session loginSession, project, raw, kind string) {
	id := rand.Text()
	now := time.Now()
	p.mu.Lock()
	if p.tokenResults == nil {
		p.tokenResults = map[string]issuedTokenResult{}
	}
	p.expireTokenResults(now)
	if len(p.tokenResults) >= 256 {
		var oldest string
		for key, result := range p.tokenResults {
			if oldest == "" || result.expires.Before(p.tokenResults[oldest].expires) {
				oldest = key
			}
		}
		delete(p.tokenResults, oldest)
	}
	p.tokenResults[id] = issuedTokenResult{kind: kind, raw: raw, csrf: session.csrf, project: project, expires: now.Add(2 * time.Minute)}
	p.mu.Unlock()
	w.Header().Set("Referrer-Policy", "no-referrer")
	target := projectURL("settings", project) + "&issued=" + url.QueryEscape(id)
	if kind == "user" {
		target = "/profile/api-tokens?issued=" + url.QueryEscape(id)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// Called only after the settings view has verified current project permissions.
func (p *panel) takeIssuedToken(r *http.Request, session loginSession, project string) string {
	return p.takeIssuedTokenKind(r, session, project, "project")
}

func (p *panel) takeIssuedTokenKind(r *http.Request, session loginSession, project, kind string) string {
	if r.Method != http.MethodGet {
		return ""
	}
	ids := r.URL.Query()["issued"]
	if len(ids) != 1 || ids[0] == "" {
		return ""
	}
	p.mu.Lock()
	p.expireTokenResults(time.Now())
	result, ok := p.tokenResults[ids[0]]
	if !ok || result.csrf != session.csrf || result.project != project || result.kind != kind {
		p.mu.Unlock()
		return ""
	}
	delete(p.tokenResults, ids[0])
	p.mu.Unlock()
	if kind == "user" && p.plaintext(r) && !p.store.InstanceHTTPPolicy() {
		return ""
	}
	return result.raw
}

// p.mu must be held.
func (p *panel) expireTokenResults(now time.Time) {
	for id, result := range p.tokenResults {
		if !now.Before(result.expires) {
			delete(p.tokenResults, id)
		}
	}
}
