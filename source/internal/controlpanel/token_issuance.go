package controlpanel

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

const maxAutoBindingLimit = 100000
const maxAutoLifetimeDays = 36500

var errScopeUpgradeRequired = errors.New("scoped token issuance requires client upgrade and validation")

// ProjectTokenOptions is copied and validated before an immutable issuance
// snapshot is committed. Nil Scopes preserves the historical LEGACY_V1 default;
// an explicit empty set is invalid. Limits apply only to auto admission.
type ProjectTokenOptions struct {
	Scopes                []string
	MaxAutoBindings       int
	AutoLifetimeDays      int
	UnlimitedAutoBindings bool
	UnlimitedAutoLifetime bool
}

func (s *Store) projectTokenMetadataLocked(actor, project string, o ProjectTokenOptions) (SessionToken, error) {
	scopes := slices.Clone(o.Scopes)
	if scopes == nil {
		scopes = access.LegacyV1Scopes()
	}
	slices.Sort(scopes)
	if !access.IssuableScopes(scopes) || !access.HasScope(scopes, access.SessionRegister) {
		return SessionToken{}, ErrInvalid
	}
	auto := access.HasScope(scopes, access.SessionRegisterAuto)
	if auto && s.projectRoleLocked(actor, project) != "owner" {
		return SessionToken{}, ErrForbidden
	}
	if !bool(s.data.ScopeIssuanceReady) && !slices.Equal(scopes, access.LegacyV1Scopes()) {
		return SessionToken{}, errScopeUpgradeRequired
	}
	t := SessionToken{ScopeVersion: access.ScopeVersion, Scopes: scopes, IssuedBy: actor, AdmissionMode: "manual", IssuedAt: s.now().UTC()}
	if !auto {
		if o.MaxAutoBindings != 0 || o.AutoLifetimeDays != 0 || o.UnlimitedAutoBindings || o.UnlimitedAutoLifetime {
			return SessionToken{}, ErrInvalid
		}
		return t, nil
	}
	// Zero options preserve historical defaults; only explicit choices are unlimited.
	if o.UnlimitedAutoBindings && o.MaxAutoBindings != 0 || o.UnlimitedAutoLifetime && o.AutoLifetimeDays != 0 {
		return SessionToken{}, ErrInvalid
	}
	if o.MaxAutoBindings == 0 && !o.UnlimitedAutoBindings {
		o.MaxAutoBindings = 8
	}
	if o.AutoLifetimeDays == 0 && !o.UnlimitedAutoLifetime {
		o.AutoLifetimeDays = 30
	}
	if o.MaxAutoBindings < 0 || o.MaxAutoBindings > maxAutoBindingLimit || o.AutoLifetimeDays < 0 || o.AutoLifetimeDays > maxAutoLifetimeDays {
		return SessionToken{}, ErrInvalid
	}
	t.AdmissionMode, t.MaxAutoBindings = "auto", o.MaxAutoBindings
	if !o.UnlimitedAutoLifetime {
		t.ExpiresAt = t.IssuedAt.Add(time.Duration(o.AutoLifetimeDays) * 24 * time.Hour)
	}
	return t, nil
}

func (s *Store) scopedIssuanceReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return bool(s.data.ScopeIssuanceReady)
}

// Repeated scope controls are expected, but repeated values and repeated scalar
// keys are not. An explicit empty selection never falls back to LEGACY_V1.
func projectTokenForm(form url.Values) (ProjectTokenOptions, error) {
	for key, values := range form {
		if key == "scope" {
			continue
		}
		switch key {
		case "csrf", "issuance_token", "project", "label", "ownerID", "scope_selection", "auto_cap", "auto_days":
		default:
			return ProjectTokenOptions{}, ErrInvalid
		}
		if len(values) != 1 {
			return ProjectTokenOptions{}, ErrInvalid
		}
	}
	o := ProjectTokenOptions{}
	_, hasScopes := form["scope"]
	_, hasSelection := form["scope_selection"]
	if hasSelection || hasScopes {
		if form.Get("scope_selection") != "custom" {
			return o, ErrInvalid
		}
		o.Scopes = append([]string{}, form["scope"]...)
	}
	for key, target := range map[string]*int{"auto_cap": &o.MaxAutoBindings, "auto_days": &o.AutoLifetimeDays} {
		if values, ok := form[key]; ok {
			if values[0] == "unlimited" || values[0] == "0" {
				if key == "auto_cap" {
					o.UnlimitedAutoBindings = true
				} else {
					o.UnlimitedAutoLifetime = true
				}
				continue
			}
			v, err := strconv.Atoi(values[0])
			if err != nil || v < 1 {
				return o, ErrInvalid
			}
			*target = v
		}
	}
	return o, nil
}

// Missing metadata on an existing v4 manual token remains compatible. Newly
// issued auto tokens use zero limits for explicit unlimited admission.
func validTokenAdmission(t SessionToken, version int) bool {
	if version < scopeIntroductionVersion {
		return t.AdmissionMode == "" && t.IssuedBy == "" && t.MaxAutoBindings == 0
	}
	auto := access.HasScope(t.Scopes, access.SessionRegisterAuto)
	if auto {
		duration := t.ExpiresAt.Sub(t.IssuedAt)
		return t.Kind == tokenProject && t.AdmissionMode == "auto" && t.IssuedBy == t.OwnerID && t.MaxAutoBindings >= 0 && t.MaxAutoBindings <= maxAutoBindingLimit && (t.ExpiresAt.IsZero() || (duration >= 24*time.Hour && duration <= maxAutoLifetimeDays*24*time.Hour))
	}
	return (t.AdmissionMode == "" || (t.Kind == tokenProject && t.AdmissionMode == "manual") || (t.Kind == tokenSession && t.AdmissionMode == "auto" && len(t.ParentID) == 52)) && t.MaxAutoBindings == 0 && (t.IssuedBy == "" || t.IssuedBy == t.OwnerID)
}
