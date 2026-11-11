package controlpanel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
)

var errSettingsConflict = errors.New("settings changed")

const settingsConflictMessage = "error.settings_conflict"

// Version only the fields this form replaces, with a form and target namespace.
// Values come from the rendered snapshot; nothing is persisted or authenticated
// by this digest. In particular, credentials never belong in these values.
func settingsVersion(kind, target string, values ...string) string {
	raw, _ := json.Marshal(append([]string{kind, target}, values...))
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func projectSettingsVersion(p Project) string {
	return settingsVersion("project-access", p.Name, p.Mode, strconv.FormatBool(p.Enabled))
}
func userSettingsVersion(u User) string {
	return settingsVersion("user-access", u.ID, u.Role, strconv.FormatBool(u.Enabled))
}
func profileSettingsVersion(u User, language string) string {
	return settingsVersion("profile", u.ID, u.DisplayName, u.Email, language)
}
func policySettingsVersion(kind string, enabled bool) string {
	return settingsVersion(kind, "instance", strconv.FormatBool(enabled))
}

// Call under the same store lock as the mutation, after authorization. Omitting
// the field preserves the legacy API; an explicitly empty or repeated field
// does not. Equality to the canonical hex digest also rejects malformed values.
func checkSettingsVersion(provided []string, current string) error {
	if len(provided) == 0 {
		return nil
	}
	if len(provided) != 1 || provided[0] != current {
		return errSettingsConflict
	}
	return nil
}
