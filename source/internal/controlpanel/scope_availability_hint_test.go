package controlpanel

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestScopeAvailabilityHintByRoleAndReadiness(t *testing.T) {
	s := userStore(t)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	for _, tc := range []struct {
		name, user string
		ready      bool
	}{
		{"admin-off", "admin", false}, {"owner-off", "owner", false},
		{"admin-on", "admin", true}, {"owner-on", "owner", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.data.ScopeIssuanceReady = policyBool(tc.ready)
			response := loginPanelUser(t, h, tc.user).call("GET", "/projects/settings?project=demo", nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d", response.Code)
			}
			body := response.Body.String()
			fieldset := regexp.MustCompile(`(?s)<fieldset class="token-scope-options"[^>]*>.*?</fieldset>`).FindString(body)
			if !strings.Contains(fieldset, `aria-describedby="scope-availability"`) {
				t.Error("scope fieldset lacks availability description")
			}
			if strings.Contains(body, "LEGACY_V1") || strings.Contains(body, en("error.token_scope_upgrade")) {
				t.Error("old long notice exposed")
			}
			hint := regexp.MustCompile(`<p id="scope-availability" class="form-hint">(.*?)</p>`).FindStringSubmatch(fieldset)
			if len(hint) != 2 {
				t.Fatal("scope availability hint missing")
			}
			ready := en("tokens.after_issuance_change_ordinary_scopes_with_edit")
			admin := en("tokens.restricted_off_admin_before") + `<a href="/admin/settings">` + en("tokens.instance_settings") + `</a>` + en("tokens.restricted_off_admin_after")
			owner := en("tokens.restricted_off_user")
			want := owner
			if tc.ready {
				want = ready
			} else if tc.user == "admin" {
				want = admin
			}
			if hint[1] != want {
				t.Errorf("hint=%q, want %q", hint[1], want)
			}
			if tc.ready && (strings.Contains(fieldset, en("tokens.restricted_off_user")) || strings.Contains(fieldset, strings.TrimSpace(en("tokens.restricted_off_admin_before")))) {
				t.Error("off hint shown while ready")
			}
			header, _, _ := strings.Cut(fieldset, ">")
			if (!tc.ready) != strings.Contains(header, " disabled") {
				t.Error("scope gate disabled state changed")
			}
		})
	}
}
