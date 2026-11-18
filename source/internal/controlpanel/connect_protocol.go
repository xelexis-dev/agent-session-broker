package controlpanel

import (
	"errors"
	"maps"
	"net/http"
	"strings"
)

var errConnectHTTPDenied = deviceError("connect_http_denied")

func (s *Store) connectCapability() (enabled, httpAllowed bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	enabled = !s.closed && s.data.Admin != nil && s.connect.enabled
	return enabled, enabled && s.data.AllowHTTP
}

func withConnectParent(response DeviceTokenResponse, parent userAPIToken, p Project) DeviceTokenResponse {
	response.ParentKind = "user-connect"
	response.ParentExpiresAt = parent.ExpiresAt
	response.Project = p.Name
	response.ProjectGeneration = p.ProjectGenerationID
	return response
}

// A raw connect bearer on /mcp is an old-client misuse, regardless of switch,
// project, proxy or transport. Match independently of connect eligibility.
func (s *Store) revokeConnectOnMCP(raw string) (bool, error) {
	if !strings.HasPrefix(raw, "asb_u_") || len(raw) != 58 {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := digest(raw)
	for id, t := range s.api.tokens {
		if !isConnectToken(t) || !equal(t.Hash, hash) {
			continue
		}
		tokens := maps.Clone(s.api.tokens)
		delete(tokens, id)
		event := connectEvent(t, Project{}, "", "token-revoked", "wrong-endpoint", "")
		return true, s.commitConnect(s.data, nil, nil, nil, tokens, []connectAudit{event}, nil)
	}
	return false, nil
}
func (p *panel) wrongConnectEndpoint(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != panelRouteMcp {
		return false
	}
	matched, err := p.store.revokeConnectOnMCP(bearerToken(r))
	if !matched {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err != nil {
		writeDeviceError(w, http.StatusServiceUnavailable, "server_error")
	} else {
		writeDeviceError(w, http.StatusUnauthorized, string(errConnectForbidden))
	}
	return true
}

// Handle only connect-specific failures; existing project-token responses keep
// their historical status, headers and JSON shape.
func writeConnectFailure(w http.ResponseWriter, err error) bool {
	status, code := http.StatusForbidden, string(errConnectForbidden)
	switch {
	case errors.Is(err, errConnectDisabled):
		code = string(errConnectDisabled)
	case errors.Is(err, errConnectHTTPDenied):
		code = string(errConnectHTTPDenied)
	case errors.Is(err, errConnectCapacity):
		status, code = http.StatusConflict, string(errConnectCapacity)
	case errors.Is(err, errConnectForbidden):
	case errors.Is(err, errAPIUnauthorized):
		status = http.StatusUnauthorized
	default:
		return false
	}
	writeDeviceError(w, status, code)
	return true
}
