package controlpanel

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSessionDisplayStateUsesOneRule(t *testing.T) {
	// PM decision: detached > suspended > ready (even with an error code) >
	// error code > waiting. ProbePending only adds a note.
	for _, tc := range []struct {
		name    string
		session Session
		want    string
	}{
		{"ready", Session{Ready: true}, "ready"},
		{"ready despite an error code", Session{Ready: true, ErrorCode: "ingress-failed"}, "ready"},
		{"error code without ready", Session{Status: "missing", ErrorCode: "server-restarted"}, "unreachable"},
		{"neither ready nor failing", Session{Status: "alive"}, "waiting"},
		{"probe pending keeps waiting", Session{ProbePending: true}, "waiting"},
		{"probe pending keeps ready", Session{Ready: true, ProbePending: true}, "ready"},
		{"suspended wins over ready", Session{Ready: true, Suspended: true}, "suspended"},
		{"suspended wins over errors", Session{Suspended: true, ErrorCode: "suspended"}, "suspended"},
		{"detached wins over everything", Session{Ready: true, Suspended: true, ErrorCode: "detached"}, "detached"},
	} {
		if got := tc.session.DisplayState(); got != tc.want {
			t.Errorf("%s: DisplayState() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSessionSummaryExcludesDetachedAndCountsWaitingAsUnreachable(t *testing.T) {
	got := summarizeSessions([]Session{
		{Ready: true},
		{Ready: true, ErrorCode: "ingress-failed"},
		{Status: "missing", ErrorCode: "server-restarted"},
		{Status: "alive"},
		{Ready: true, Suspended: true},
		{ErrorCode: "detached"},
	})
	if got != (sessionSummary{Registered: 5, Ready: 2, Unreachable: 2, Suspended: 1}) {
		t.Fatalf("summary = %+v", got)
	}
	if summarizeSessions(nil) != (sessionSummary{}) {
		t.Fatal("empty summary is not zero")
	}
}

func TestSessionOwnersRetainRevokedBoundTokens(t *testing.T) {
	s := userStore(t)
	identity := `["codex","11111111-2222-3333-4444-555555555555","pc-client-a"]`
	revoked := `["claude","66666666-7777-8888-9999-000000000000",""]`
	p := s.data.Projects["demo"]
	p.Tokens = append(p.Tokens,
		SessionToken{Kind: tokenSession, ID: "grant-a", OwnerID: "developer", BoundIdentity: identity},
		SessionToken{Kind: tokenSession, ID: "grant-b", OwnerID: "viewer", BoundIdentity: revoked, Revoked: true},
	)
	s.data.Projects["demo"] = p
	owners := s.SessionOwners("demo")
	if owners[identity] != "developer" {
		t.Errorf("bound owner = %q", owners[identity])
	}
	if owners[revoked] != "viewer" {
		t.Error("revocation erased displayed ownership")
	}
	if len(s.SessionOwners("missing-project")) != 0 {
		t.Error("unknown project has owners")
	}
}

func TestSessionsPageShowsSummaryOwnerAndKeepsActions(t *testing.T) {
	s := userStore(t)
	p := s.data.Projects["demo"]
	p.Tokens = append(p.Tokens, SessionToken{Kind: tokenSession, ID: "grant-a", OwnerID: "developer", BoundIdentity: `["codex","11111111-2222-3333-4444-555555555555","pc-client-a"]`, IssuedAt: time.Now(), LastUsedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	s.data.Projects["demo"] = p
	h := New(s, layoutBackend{}, http.NotFoundHandler())
	body := loginPanelUser(t, h, "owner").call("GET", "/projects/sessions?project=demo", nil).Body.String()
	for _, want := range []string{
		`<span class="metric__label">` + en("sessions.metric_registered") + `</span><strong class="metric__value" data-session-summary="Registered">1</strong>`,
		`<span class="metric__label">` + en("session.connection.ready") + `</span>`,
		`<span class="metric__label">` + en("session.connection.unreachable") + `</span>`,
		`<span class="metric__label">` + en("session.connection.suspended") + `</span>`,
		`data-label="` + en("sessions.col_account") + `">developer<`,
		`data-label="` + en("sessions.col_account") + `"><span class="muted" aria-label="` + en("sessions.owner_unknown") + `">—</span>`,
		`action="/sessions/suspend"`, `action="/sessions/detach"`,
		`data-table-filter="session-table"`, `<caption class="sr-only">` + en("sessions.caption") + `</caption>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sessions page lacks %s", want)
		}
	}
	tables := layoutTables(t, body)
	if got := strings.Join(tables[en("sessions.caption")].headers, "|"); got != strings.Join([]string{en("sessions.col_account"), en("sessions.col_session"), en("sessions.col_harness"), en("sessions.col_status"), en("sessions.col_manage")}, "|") {
		t.Errorf("session headers = %s", got)
	}
}
