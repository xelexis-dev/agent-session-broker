package controlpanel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// readyBackend serves the same rows to the page and to live polling.
type readyBackend struct{ liveFixtureBackend }

func (b *readyBackend) Sessions(context.Context, string) ([]Session, error) { return b.rows, nil }

func readySessionBackend() *readyBackend {
	now := time.Now()
	return &readyBackend{liveFixtureBackend{rows: []Session{{ID: "fixture", RegistrationID: "fixture", Name: "session", Status: "alive", Ready: true,
		Activity: &protocol.ActivitySnapshot{Activity: protocol.Activity{State: protocol.ActivityWorking, Source: "claude-session-file", Sequence: 1, ObservedAt: now},
			ReceivedAt: now, ExpiresAt: now.Add(time.Minute)}}}}}
}

// MR-4: the sessions page follows the account language, and live updates follow the polling page's language.
func TestSessionLabelsFollowTheLanguage(t *testing.T) {
	h := New(userStore(t), readySessionBackend(), http.NotFoundHandler())
	page := loginPanelUser(t, h, "owner").call("GET", "/projects/sessions?project=demo", nil).Body.String()
	for _, want := range []string{"Ready to receive", "Activity: Working", "Claude session status"} {
		if !strings.Contains(page, want) {
			t.Errorf("English sessions page lacks %q", want)
		}
	}
	for _, korean := range []string{"수신 준비", "턴 진행", "연결된 세션"} {
		if strings.Contains(page, korean) {
			t.Errorf("English sessions page still shows %q", korean)
		}
	}
	for query, want := range map[string][2]string{"&lang=en": {"Ready to receive", "Working"}, "&lang=ko": {"수신 준비", "턴 진행"}, "": {"수신 준비", "턴 진행"}} {
		w := loginPanelUser(t, h, "owner").call("GET", "/projects/sessions/live?project=demo"+query, nil)
		var result struct{ Rows []liveSessionRow }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Rows) != 1 {
			t.Fatalf("live %q: %d", query, w.Code)
		}
		if got := [2]string{result.Rows[0].ConnectionLabel, result.Rows[0].Activity.Label}; got != want {
			t.Errorf("live %q labels %q, want %q", query, got, want)
		}
	}
}

// session-live.js repeats the labels it rebuilds in the browser (expiry, tooltip lines); they must match the server's.
func TestLiveScriptLabelsMatchTheServer(t *testing.T) {
	pairs := map[string]string{
		"js.sessions.activity.working": "session.activity.working", "js.sessions.activity.idle": "session.activity.idle",
		"js.sessions.activity.none": "session.activity.none", "js.sessions.activity.unknown": "session.activity.unknown",
		"js.sessions.activity.expired_reason": "session.activity.reason.observation-expired",
		"js.sessions.activity_aria":           "sessions.activity_aria",
		"js.sessions.runtime.unknown":         "session.runtime.unknown", "js.sessions.runtime.unknown_part": "session.runtime.unknown_part",
		"js.sessions.tooltip.activity": "session.tooltip.activity", "js.sessions.tooltip.activity_none": "session.tooltip.activity_none",
		"js.sessions.tooltip.source": "session.tooltip.source", "js.sessions.tooltip.reason": "session.tooltip.reason",
		"js.sessions.tooltip.observed_at": "session.tooltip.observed_at", "js.sessions.tooltip.changed_at": "session.tooltip.changed_at",
		"js.sessions.tooltip.received_at": "session.tooltip.received_at", "js.sessions.tooltip.expires_at": "session.tooltip.expires_at",
		"js.sessions.tooltip.probe_pending": "session.tooltip.probe_pending",
	}
	for _, language := range i18n.Languages {
		for js, server := range pairs {
			a, okA := i18n.Lookup(language, js)
			b, okB := i18n.Lookup(language, server)
			if !okA || !okB || a != b {
				t.Errorf("%s: %s=%q differs from %s=%q", language, js, a.Other, server, b.Other)
			}
		}
	}
}

// The session pages are fully in the catalog: an English account sees no Korean outside the message block, including
// audit rows, reasons and the runtime/activity tooltips.
func TestSessionPagesHaveNoKoreanInEnglish(t *testing.T) {
	s := userStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	generation := s.data.Projects["demo"].ProjectGenerationID
	for i, kind := range []string{"request", "grant", "registration", "rejected", "revoked", "expired", "detached", "suspended", "unsuspended", "handover"} {
		s.audits[kind] = AuditEvent{Project: "demo", ProjectGenerationID: generation, Kind: kind, Reason: "fixture", At: time.Now().UTC().Add(-time.Duration(i) * time.Second)}
	}
	s.mu.Unlock()
	backend := readySessionBackend()
	backend.rows = append(backend.rows, Session{ID: "lost", Name: "lost", Status: "missing", ErrorCode: "ingress-failed", ProbePending: true}, Session{ID: "held", Name: "held", Status: "alive", Suspended: true, ErrorCode: "suspended"})
	owner := loginPanelUser(t, New(s, backend, http.NotFoundHandler()), "owner")
	for path, rows := range map[string]string{"/projects/sessions?project=demo": "data-session-row=", "/projects/audit?project=demo": "data-audit-kind="} {
		w := owner.call("GET", path, nil)
		outside := dataBlockRE.ReplaceAllString(w.Body.String(), "")
		if n := strings.Count(outside, rows); n < 3 {
			t.Errorf("%s: only %d rows rendered", path, n)
		}
		if w.Code != http.StatusOK || hangulRE.MatchString(outside) {
			t.Errorf("%s: status %d, Korean %q", path, w.Code, hangulRE.FindString(outside))
		}
	}
}
