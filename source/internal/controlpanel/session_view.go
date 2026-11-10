package controlpanel

// DisplayState is the one rule the session badge and the summary share
// (PM decision): a detached registration is history, an administrator's
// suspension comes next, a ready session can receive even if it reports an
// error code, an error code without readiness cannot be confirmed, and the rest
// is still waiting. ProbePending only adds a note; the protocol Status and
// StateReason stay visible beside the badge.
func (s Session) DisplayState() string {
	switch {
	case s.GrantID != "":
		return "unregistered"
	case s.ErrorCode == "detached":
		return "detached"
	case s.Suspended:
		return "suspended"
	case s.Ready:
		return "ready"
	case s.ErrorCode != "":
		return "unreachable"
	default:
		return "waiting"
	}
}

// sessionSummary backs the four summary cards. Unreachable ("확인 불가")
// includes waiting sessions; their rows still show the separate 대기 badge.
type sessionSummary struct {
	Registered, Ready, Unreachable, Suspended, Reserved int
}

func summarizeSessions(sessions []Session) sessionSummary {
	var sum sessionSummary
	for _, s := range sessions {
		switch s.DisplayState() {
		case "unregistered":
			sum.Reserved++
			continue
		case "detached":
			continue
		case "ready":
			sum.Ready++
		case "suspended":
			sum.Suspended++
		default:
			sum.Unreachable++
		}
		sum.Registered++
	}
	return sum
}
