package controlpanel

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSessionLiveRequiresPermissionWithoutRedirect(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/projects/sessions/live?project=demo", nil))
	if w.Code != http.StatusUnauthorized || w.Header().Get("Location") != "" {
		t.Fatalf("anonymous live status=%d location=%q, want 401 without redirect", w.Code, w.Header().Get("Location"))
	}
}

type liveFixtureBackend struct {
	panelBackend
	rows []Session
	read func()
}

func (b *liveFixtureBackend) LiveSessions(context.Context, string) ([]Session, error) {
	if b.read != nil {
		b.read()
	}
	return b.rows, nil
}

func TestSessionLiveRolesAndRevocation(t *testing.T) {
	s := userStore(t)
	now := time.Now()
	b := &liveFixtureBackend{rows: []Session{{ID: "fixture", RegistrationID: "fixture", Name: "session", Status: "alive", Ready: true, Activity: &protocol.ActivitySnapshot{Activity: protocol.Activity{State: protocol.ActivityWorking, Source: "claude-session-file", Sequence: 1, ObservedAt: now}, ReceivedAt: now, ExpiresAt: now.Add(15 * time.Second)}}}}
	h := New(s, b, http.NotFoundHandler())
	for _, tc := range []struct {
		user   string
		status int
		manage bool
	}{{"owner", 200, true}, {"developer", 200, false}, {"viewer", 200, false}, {"outsider", 403, false}} {
		t.Run(tc.user, func(t *testing.T) {
			c := loginPanelUser(t, h, tc.user)
			w := c.call("GET", "/projects/sessions/live?project=demo", nil)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if w.Header().Get("Location") != "" || !strings.Contains(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") || len(w.Result().Cookies()) != 0 {
				t.Fatal("live request changed cookies or response contract")
			}
			if w.Code == 200 {
				var result struct{ Rows []liveSessionRow }
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if len(result.Rows) != 1 || result.Rows[0].Activity.State != "working" || result.Rows[0].CanManage != tc.manage {
					t.Fatalf("rows=%+v", result.Rows)
				}
			}
		})
	}
	dev := loginPanelUser(t, h, "developer")
	b.read = func() {
		if err := s.SetProjectMember("owner", "demo", "developer", ""); err != nil {
			t.Fatal(err)
		}
	}
	if w := dev.call("GET", "/projects/sessions/live?project=demo", nil); w.Code != 403 {
		t.Fatalf("removed during read=%d", w.Code)
	}
	b.read = nil
	viewer := loginPanelUser(t, h, "viewer")
	if err := s.UpdateUser("admin", "viewer", "user", false, ""); err != nil {
		t.Fatal(err)
	}
	if w := viewer.call("GET", "/projects/sessions/live?project=demo", nil); w.Code != 401 {
		t.Fatalf("disabled=%d", w.Code)
	}
	owner := loginPanelUser(t, h, "owner")
	for _, tc := range []struct {
		path string
		want int
	}{{"", 400}, {"?project=demo&project=other", 400}, {"?project=absent", 404}} {
		if w := owner.call("GET", "/projects/sessions/live"+tc.path, nil); w.Code != tc.want {
			t.Errorf("%s=%d want%d", tc.path, w.Code, tc.want)
		}
	}
	if w := owner.call("POST", "/projects/sessions/live?project=demo", nil); w.Code != 405 {
		t.Fatalf("post=%d", w.Code)
	}
	if b.detached != "" {
		t.Fatal("read detached a registration")
	}
}

func TestSessionActivityPresentationDoesNotChangeConnection(t *testing.T) {
	now := time.Now()
	a := &protocol.ActivitySnapshot{Activity: protocol.Activity{State: protocol.ActivityIdle, Source: "codex-rollout", Reason: "untrusted-raw-marker", ObservedAt: now}, ReceivedAt: now, ExpiresAt: now.Add(time.Minute)}
	base := Session{ID: "same", RegistrationID: "same", Ready: true, Status: "alive", Activity: a}
	for _, tc := range []struct {
		name   string
		change func(*Session)
		want   string
	}{{"idle", func(*Session) {}, "idle"}, {"missing", func(s *Session) { s.Status = "missing" }, "unknown"}, {"suspended", func(s *Session) { s.Suspended = true }, "unknown"}, {"grant", func(s *Session) { s.GrantID = "same" }, "none"}, {"expired", func(s *Session) { copy := *a; copy.ExpiresAt = now.Add(-time.Second); s.Activity = &copy }, "unknown"}, {"old-client", func(s *Session) { s.Activity = nil }, "unknown"}} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.change(&s)
			before := s.DisplayState()
			v := s.ActivityView("en")
			if v.State != tc.want || s.DisplayState() != before || strings.Contains(v.Reason, "untrusted") {
				t.Fatalf("view=%+v", v)
			}
		})
	}
	grant := base
	grant.GrantID = "same"
	legacy := base
	legacy.RegistrationID = ""
	if base.RowKey() == grant.RowKey() || base.RowKey() == legacy.RowKey() || grant.RowKey() == legacy.RowKey() {
		t.Fatal("row key namespaces collide")
	}
	if base.RowShape(true, "owner") == base.RowShape(false, "owner") {
		t.Fatal("management changes must require reload")
	}
}

func TestSessionLiveDoesNotPersistExpirationHousekeeping(t *testing.T) {
	s := userStore(t)
	h := New(s, &liveFixtureBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "owner")
	s.mu.Lock()
	p := s.data.Projects["demo"]
	p.Tokens = append(p.Tokens, SessionToken{Kind: tokenSession, ID: "expired-fixture", OwnerID: "owner", Nickname: "expired-reservation", IssuedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute)})
	s.data.Projects["demo"] = p
	before, _ := json.Marshal(s.data)
	s.mu.Unlock()
	w := c.call("GET", "/projects/sessions/live?project=demo", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	s.mu.RLock()
	after, _ := json.Marshal(s.data)
	s.mu.RUnlock()
	if string(before) != string(after) {
		t.Fatal("live GET persisted expiration housekeeping")
	}
	if strings.Contains(w.Body.String(), "expired-fixture") {
		t.Fatal("expired reservation shown")
	}
}

func TestSessionActivityIsTooltipOnlyWithAccessibleIndicator(t *testing.T) {
	now := time.Now()
	for _, state := range []protocol.ActivityState{protocol.ActivityWorking, protocol.ActivityIdle, protocol.ActivityUnknown} {
		t.Run(string(state), func(t *testing.T) {
			w := httptest.NewRecorder()
			session := Session{ID: "fixture", Name: "fixture", Ready: true, Status: "alive", Activity: &protocol.ActivitySnapshot{Activity: protocol.Activity{State: state, Source: "codex-rollout"}, ReceivedAt: now, ExpiresAt: now.Add(time.Minute)}}
			if err := pages.ExecuteTemplate(w, "sessions", page{View: "sessions", Project: "demo", Sessions: []Session{session, {ID: "grant:fixture", GrantID: "fixture", Name: "reserved"}}, CanManage: map[string]bool{}, Owners: map[string]string{}}); err != nil {
				t.Fatal(err)
			}
			body := w.Body.String()
			triggers := regexp.MustCompile(`(?s)class="asb-tooltip__trigger"[^>]*>(.*?)</button>`).FindAllStringSubmatch(body, -1)
			if len(triggers) != 2 {
				t.Fatalf("triggers=%d", len(triggers))
			}
			visible := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(triggers[0][1], "")
			for _, word := range []string{en("session.activity.working"), en("session.activity.unknown"), "—"} {
				if strings.Contains(visible, word) {
					t.Fatalf("activity wording visible: %q", visible)
				}
			}
			if strings.Contains(visible, en("session.activity.idle")) {
				t.Fatalf("activity wording visible: %q", visible)
			}
			if !strings.Contains(triggers[0][1], `role="img" aria-label="`+enHTML("sessions.activity_aria", "label", activityLabel(string(state)).In("en"))+`"`) {
				t.Fatal("accessible activity label missing")
			}
			if strings.Contains(triggers[1][1], "data-activity-state") {
				t.Fatal("reserved grant has an activity indicator")
			}
			if !strings.Contains(body, `<span data-activity-line>`+enHTML("session.tooltip.activity", "label", activityLabel(string(state)).In("en"))+`</span>`) {
				t.Fatal("activity tooltip missing")
			}
			header := regexp.MustCompile(`(?s)<div class="section-heading panel__header">(.*?)<small class="autocomplete-status"`).FindStringSubmatch(body)
			if len(header) != 2 || !strings.Contains(header[1], "data-session-live-status") {
				t.Fatal("live notice is outside padded header")
			}
		})
	}
}

func TestSessionActivityTooltipSemanticTimes(t *testing.T) {
	observed, _ := time.Parse(time.RFC3339Nano, "2026-10-10T07:19:28.316438Z")
	session := Session{ID: "fixture", Name: "fixture", Ready: true, Status: "alive", Activity: &protocol.ActivitySnapshot{Activity: protocol.Activity{State: protocol.ActivityUnknown, ObservedAt: observed}, ReceivedAt: observed}}
	w := httptest.NewRecorder()
	if err := pages.ExecuteTemplate(w, "sessions", page{View: "sessions", Project: "demo", Sessions: []Session{session}, CanManage: map[string]bool{}, Owners: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<time datetime="2026-10-10T07:19:28.316438Z" data-activity-time="observedAt">2026-10-10 07:19:28 UTC</time>`) {
		t.Fatal("original datetime or human-readable no-JS fallback missing")
	}
	if strings.Contains(body, `data-activity-time="changedAt"`) || strings.Contains(body, `data-activity-time="expiresAt"`) {
		t.Fatal("zero timestamps rendered")
	}
	visible := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(body, "")
	if strings.Contains(visible, observed.Format(time.RFC3339Nano)) {
		t.Fatal("raw ISO visible outside machine attributes")
	}
	session.GrantID = "reserved"
	if len(session.ActivityTooltipTimes("en")) != 0 {
		t.Fatal("reserved grant exposes activity timestamps")
	}
	session.GrantID = ""
	session.Status = "missing"
	if len(session.ActivityTooltipTimes("en")) != 0 {
		t.Fatal("missing connection exposes previous binding times")
	}
}
