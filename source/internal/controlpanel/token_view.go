package controlpanel

import "time"

// tokenView presents one project token with values derived from its session
// grants and from the sessions the backend reports as connected right now.
type tokenView struct {
	CanEditScopes, ScopeSelected bool
	SessionToken
	LastUsed time.Time
	Live     int
	State    string
}

// sessionConnected is the presentation rule for "currently connected": a row
// the backend still observes, not merely a registration that once existed.
func sessionConnected(s Session) bool {
	return s.ErrorCode == "" && s.Status != "" && s.Status != "missing" && s.Status != "stalled"
}

// projectTokenViews keeps usable project tokens the viewer may see. The active
// state requires a session the backend reports now, not an unexpired grant,
// while approval and registration history survive revocation and expiry.
func projectTokenViews(tokens []SessionToken, sessions []Session, visible func(SessionToken) bool) []tokenView {
	connected := map[string]bool{}
	for _, s := range sessions {
		if sessionConnected(s) {
			connected[sessionIdentity(s)] = true
		}
	}
	var views []tokenView
	for _, token := range tokens {
		if token.Kind != tokenProject || token.Revoked || !visible(token) {
			continue
		}
		v := tokenView{SessionToken: token, LastUsed: token.LastUsedAt}
		for _, grant := range tokens {
			if grant.ParentID != token.ID {
				continue
			}
			if grant.LastUsedAt.After(v.LastUsed) {
				v.LastUsed = grant.LastUsedAt
			}
			if !grant.Revoked && grant.ExpiredAt.IsZero() && connected[grant.BoundIdentity] {
				v.Live++
			}
		}
		switch {
		case v.Live > 0:
			v.State = "active"
		case !token.LastRegisteredAt.IsZero():
			v.State = "registered"
		default:
			v.State = "idle"
		}
		views = append(views, v)
	}
	return views
}
