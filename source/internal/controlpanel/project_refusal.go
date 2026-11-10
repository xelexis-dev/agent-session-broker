package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
)

// Hints classify a refusal only; successful authentication always wins.
func (p *panel) writeProjectRefusal(w http.ResponseWriter, r *http.Request, project string) bool {
	p.store.mu.Lock()
	current, exists := p.store.data.Projects[project]
	p.store.mu.Unlock()
	code := ""
	switch {
	case !exists:
		code = "project_not_found"
	case r.Header.Get(protocol.ProjectGenerationHeader) != "" && r.Header.Get(protocol.ProjectGenerationHeader) != current.ProjectGenerationID:
		code = "project_deleted"
	case current.Deleting:
		code = "project_deleting"
	case !current.Enabled:
		code = "project_disabled"
	}
	if code == "" {
		return false
	}
	if code == "project_deleting" || code == "project_disabled" {
		w.Header().Set("Retry-After", "30")
	}
	writeDeviceError(w, protocol.ProjectRefusalStatus(code), code)
	return true
}

func (p *panel) oauthProjectRefused(w http.ResponseWriter, r *http.Request) bool {
	if !p.store.Ready() || r.Method != http.MethodPost {
		return false
	}
	q := r.URL.Query()
	names := q["project"]
	if len(names) != 1 || !projectName.MatchString(names[0]) {
		return false
	}
	p.store.mu.Lock()
	_, _, authErr := p.store.projectTokenLocked(names[0], bearerToken(r))
	p.store.mu.Unlock()
	if authErr == nil {
		return false
	}
	return p.writeProjectRefusal(w, r, names[0])
}
