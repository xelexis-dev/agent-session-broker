package access

import "slices"

const ScopeVersion = 1

const (
	SessionRead         = "session:read"
	SessionRegister     = "session:register"
	SessionRename       = "session:rename"
	SessionManage       = "session:manage"
	SessionSpawn        = "session:spawn"
	MessageSend         = "message:send"
	MessageReceive      = "message:receive"
	MemoryRead          = "memory:read"
	MemoryWrite         = "memory:write"
	SessionRegisterAuto = "session:register:auto"
)

// LegacyV1Scopes is fixed to features available before scoped tokens. It is
// never a wildcard, never includes auto admission, and returns a private copy.
func LegacyV1Scopes() []string {
	return []string{MemoryRead, MemoryWrite, MessageReceive, MessageSend, SessionManage, SessionRead, SessionRegister, SessionRename}
}

// IssuableScopes rejects retired permissions on new issuance and edits.
func IssuableScopes(scopes []string) bool {
	return ValidScopes(scopes) && !HasScope(scopes, SessionSpawn)
}

// ValidScopes accepts only the canonical, nonempty stored representation.
// Historical session:spawn remains readable in old data and server responses.
// Missing, duplicate, unknown or reordered fields must not become full access.
func ValidScopes(scopes []string) bool {
	if len(scopes) == 0 || len(scopes) > 10 {
		return false
	}
	for i, scope := range scopes {
		if i > 0 && scopes[i-1] >= scope {
			return false
		}
		switch scope {
		case SessionRead, SessionRegister, SessionRename, SessionManage, SessionSpawn, MessageSend, MessageReceive, MemoryRead, MemoryWrite, SessionRegisterAuto:
		default:
			return false
		}
	}
	return true
}

func HasScope(scopes []string, scope string) bool { return slices.Contains(scopes, scope) }

func ScopesSubset(child, parent []string) bool {
	for _, scope := range child {
		if !HasScope(parent, scope) {
			return false
		}
	}
	return true
}

// DelegatedScopes removes admission-only and retired permissions from a child snapshot.
func DelegatedScopes(parent []string) []string {
	out := make([]string, 0, len(parent))
	for _, scope := range parent {
		if scope != SessionRegisterAuto && scope != SessionSpawn {
			out = append(out, scope)
		}
	}
	return out
}

// RestrictedScopes reports a snapshot lacking any current full permission.
// Historical session:spawn does not count, so 8- and 9-scope full grants match.
func RestrictedScopes(scopes []string) bool { return !ScopesSubset(LegacyV1Scopes(), scopes) }

// WireScopes re-adds retired session:spawn to a full snapshot sent to clients.
// shortcut: 0.15–0.19 clients treat fewer than nine scopes as restricted; remove at 2.0 when the minimum client checks RestrictedScopes.
func WireScopes(scopes []string) []string {
	out := slices.Clone(scopes)
	if !RestrictedScopes(out) && !HasScope(out, SessionSpawn) {
		out = append(out, SessionSpawn)
		slices.Sort(out)
	}
	return out
}
