package access

import (
	"os"
	"slices"
	"strings"
	"testing"
	"unicode"
)

func TestScopeCatalogMatchesREADME(t *testing.T) {
	rows := ScopeCatalog()
	if len(rows) != 9 {
		t.Fatal("scope catalog must contain nine entries")
	}
	scopes := []string{}
	for _, d := range rows {
		scopes = append(scopes, d.Scope)
		if strings.ContainsFunc(d.Label+d.Description, func(r rune) bool { return unicode.Is(unicode.Hangul, r) }) {
			t.Errorf("agent scope text is not English: %s", d.Scope)
		}
	}
	if !ValidScopes(scopes) {
		t.Fatal("catalog is not a canonical complete scope set")
	}
	raw, e := os.ReadFile("../../../docs/README_AGENT.md")
	if e != nil {
		t.Fatal(e)
	}
	for _, d := range rows {
		want := "| `" + d.Scope + "` | " + d.Label + " | " + d.Description + " |"
		if strings.Count(string(raw), want) != 1 {
			t.Errorf("README catalog row mismatch for %s", d.Scope)
		}
	}
	rows[0].Label = "changed"
	if ScopeCatalog()[0].Label == "changed" {
		t.Fatal("catalog returned shared state")
	}
	if !slices.Equal(DelegatedScopes(scopes), LegacyV1Scopes()) {
		t.Fatal("catalog adds or drops legacy permissions")
	}
}

func TestAutoRegistrationScopeExplainsExplicitConnectionRequest(t *testing.T) {
	detail := ScopeDetails([]string{SessionRegisterAuto})[0]
	if detail.Label != "Register without approval on request" {
		t.Errorf("label = %q", detail.Label)
	}
	for _, want := range []string{"Register without approval when the user asks to connect this conversation.", "Starting a new conversation alone does not register it.", "Request direct registration with a project token without human approval", "this permission is not delegated to session credentials."} {
		if !strings.Contains(detail.Description, want) {
			t.Errorf("auto scope explanation missing %q", want)
		}
	}
}

func TestSessionManageScopeExplainsOtherSessionControl(t *testing.T) {
	const want = "Disconnect this session with stop_session. Use stop_session, retire_session, suspend_session and unsuspend_session to disconnect, retire, suspend or resume other sessions the token owner can manage. retire_session retires the registration and revokes the session credentials it used, releasing the name reservation. Retiring, suspending or resuming this session itself is not allowed. Tokens owned by an enabled instance administrator can use list_bug_reports, get_bug_report and set_bug_report_status to read and handle bug reports in the current project generation."
	got := ScopeDetails([]string{SessionManage})[0]
	if got.Description != want {
		t.Errorf("session:manage description = %q", got.Description)
	}
}
