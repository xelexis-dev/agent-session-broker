package controlpanel

import (
	"maps"
	"slices"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

const controlVersion = 5
const scopeIntroductionVersion = 4
const legacyControlMaxBytes = 2 << 20
const controlMaxBytes = 8 << 20

func validScopeState(st state) bool {
	if !validAutoState(st) {
		return false
	}
	for _, p := range st.Projects {
		parents := make(map[string]SessionToken)
		for _, token := range p.Tokens {
			if !validTokenAdmission(token, st.Version) {
				return false
			}
			if st.Version < scopeIntroductionVersion {
				if token.ScopeVersion != 0 || token.Scopes != nil {
					return false
				}
				continue
			}
			if token.ScopeVersion != access.ScopeVersion || !access.ValidScopes(token.Scopes) {
				return false
			}
			if token.Kind == tokenProject {
				if !access.HasScope(token.Scopes, access.SessionRegister) {
					return false
				}
				parents[token.ID] = token
			} else if access.HasScope(token.Scopes, access.SessionRegisterAuto) {
				return false
			}
		}
		for _, token := range p.Tokens {
			// Existing orphan grants remain loadable but ineligible, as before.
			if parent, ok := parents[token.ParentID]; ok && !access.ScopesSubset(token.Scopes, parent.Scopes) {
				return false
			}
		}
		for _, d := range p.Devices {
			if st.Version < scopeIntroductionVersion {
				if d.ScopeVersion != 0 || d.Scopes != nil {
					return false
				}
			} else {
				if d.ScopeVersion != access.ScopeVersion || !access.ValidScopes(d.Scopes) || access.HasScope(d.Scopes, access.SessionRegisterAuto) {
					return false
				}
				if parent, ok := parents[d.TokenID]; ok && !access.ScopesSubset(d.Scopes, parent.Scopes) {
					return false
				}
			}
		}
	}
	return true
}

// migrateScopes publishes metadata only after one durable state commit. It
// runs before serving, never defaults missing metadata in an already-v4 DB.
func (s *Store) migrateScopes() error {
	if s.data.Version >= scopeIntroductionVersion {
		return nil
	}
	candidate := s.data
	candidate.Version = scopeIntroductionVersion
	candidate.Projects = maps.Clone(s.data.Projects)
	for name, p := range candidate.Projects {
		p.Tokens = slices.Clone(p.Tokens)
		for i := range p.Tokens {
			p.Tokens[i].ScopeVersion = access.ScopeVersion
			p.Tokens[i].Scopes = access.LegacyV1Scopes()
		}
		p.Devices = slices.Clone(p.Devices)
		for i := range p.Devices {
			p.Devices[i].ScopeVersion = access.ScopeVersion
			p.Devices[i].Scopes = access.LegacyV1Scopes()
		}
		candidate.Projects[name] = p
	}
	return s.commit(candidate)
}

// Strip retired permissions in one commit, without altering historical audit.
func (s *Store) migrateSpawnScopes() error {
	if s.data.Version < scopeIntroductionVersion {
		return nil
	}
	candidate := s.data
	candidate.Projects = maps.Clone(s.data.Projects)
	changed := false
	strip := func(scopes []string) []string {
		if !access.HasScope(scopes, access.SessionSpawn) {
			return scopes
		}
		changed = true
		return slices.DeleteFunc(slices.Clone(scopes), func(scope string) bool { return scope == access.SessionSpawn })
	}
	for name, p := range candidate.Projects {
		p.Tokens = slices.Clone(p.Tokens)
		for i := range p.Tokens {
			p.Tokens[i].Scopes = strip(p.Tokens[i].Scopes)
			if len(p.Tokens[i].Scopes) == 0 {
				return ErrStorage
			}
		}
		p.Devices = slices.Clone(p.Devices)
		for i := range p.Devices {
			p.Devices[i].Scopes = strip(p.Devices[i].Scopes)
			if len(p.Devices[i].Scopes) == 0 {
				return ErrStorage
			}
		}
		candidate.Projects[name] = p
	}
	if !changed {
		return nil
	}
	return s.commit(candidate)
}
