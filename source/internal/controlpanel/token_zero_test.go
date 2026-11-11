package controlpanel

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestAutoTokenZeroFormIssuesUnlimited(t *testing.T) {
	s := userStore(t)
	s.data.ScopeIssuanceReady = true
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	for _, tc := range []struct {
		cap, days string
		wantCap   int
		forever   bool
	}{
		{"0", "30", 0, false}, {"8", "0", 8, true}, {"0", "0", 0, true},
	} {
		t.Run(tc.cap+"-"+tc.days, func(t *testing.T) {
			form := url.Values{"project": {"demo"}, "label": {"zero-fixture"}, "scope_selection": {"custom"}, "scope": {access.SessionRegister, access.SessionRegisterAuto}, "auto_cap": {tc.cap}, "auto_days": {tc.days}}
			w := owner.call("POST", "/tokens/project", form)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("zero limit issuance status=%d, want 303", w.Code)
			}
			tokens := s.ProjectsFor("owner")[0].Tokens
			token := tokens[len(tokens)-1]
			if token.MaxAutoBindings != tc.wantCap || token.ExpiresAt.IsZero() != tc.forever {
				t.Fatal("zero limits did not persist as unlimited")
			}
			if !tc.forever && !token.ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
				t.Fatal("finite lifetime changed")
			}
		})
	}
}

func TestAutoTokenZeroFormMarkupAndFailedRender(t *testing.T) {
	s := userStore(t)
	s.data.ScopeIssuanceReady = true
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	body := owner.call("GET", "/projects/settings?project=demo", nil).Body.String()
	if strings.Contains(body, "data-unlimited") {
		t.Fatal("unlimited checkboxes remain")
	}
	for name, max := range map[string]string{"auto_cap": "100000", "auto_days": "36500"} {
		input := regexp.MustCompile(`<input[^>]*name="` + name + `"[^>]*>`).FindString(body)
		for _, want := range []string{`min="0"`, `max="` + max + `"`, `required`} {
			if !strings.Contains(input, want) {
				t.Errorf("%s input missing %s", name, want)
			}
		}
	}
	for _, hint := range []string{en("tokens.default_8_0_for_unlimited_maximum_100000"), en("tokens.default_30_days_0_for_no_expiry")} {
		if !strings.Contains(body, hint) {
			t.Error("zero limit hint missing")
		}
	}
	// Missing mandatory registration scope forces a real validation failure.
	form := url.Values{"project": {"demo"}, "scope_selection": {"custom"}, "scope": {access.SessionRegisterAuto}, "auto_cap": {"unlimited"}, "auto_days": {"unlimited"}}
	w := owner.call("POST", "/tokens/project", form)
	if w.Code != http.StatusBadRequest {
		t.Fatal("invalid scope accepted")
	}
	for _, name := range []string{"auto_cap", "auto_days"} {
		input := regexp.MustCompile(`<input[^>]*name="` + name + `"[^>]*>`).FindString(w.Body.String())
		if !strings.Contains(input, `value="0"`) || strings.Contains(input, "disabled") {
			t.Errorf("%s unlimited failed render is not enabled zero", name)
		}
	}
	if !strings.Contains(w.Body.String(), "the automatic registration limit is 0 (unlimited) or 1–100000, and the lifetime is 0 (no expiry) or 1–36500 days.") {
		t.Fatal("invalid-limit guidance does not explain zero")
	}
}
