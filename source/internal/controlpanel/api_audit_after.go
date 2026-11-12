package controlpanel

import "github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"

// apiAuditAfter records a successful API write whose mutation was committed by
// a guarded Store core. route must be a static route template, never input;
// target is a resource id (project, token id, username) and role a role name.
// shortcut: audit is not in the mutation's transaction (crash/disk-full can lose one record), upgrade by threading an audit through commitAuditedAPI if audit completeness becomes a requirement
func (s *Store) apiAuditAfter(route, actor, target, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.apiBatchLocked(map[string][]byte{}, nil, apiAuditEvent{Action: route, Actor: actor, Target: target, Role: role, Result: "success"}); err != nil {
		serverdiag.Emit(serverdiag.Event{Type: "admin-api-audit-failed", Route: staticAPIRoute(route)})
	}
}
