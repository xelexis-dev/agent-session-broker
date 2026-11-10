package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAttachmentSharesMCPAuthorizationAndTransport(t *testing.T) {
	s := userStore(t)
	raw, e := s.IssueTokenFor("owner", "demo", "sender", "owner")
	if e != nil {
		t.Fatal("fixture")
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, ok := access.FromContext(r.Context())
		if !ok || g.Project != "demo" || g.TokenID == "" {
			t.Error("authenticated grant missing")
		}
		w.WriteHeader(204)
	})
	h := New(s, &ownedPanelBackend{}, next)
	for _, path := range []string{"/attachments", "/attachments/AAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		for _, tc := range []struct {
			name, scheme, token string
			want                int
		}{{"missing", "https", "", 401}, {"wrong", "https", "wrong", 401}, {"plain", "http", raw, 403}, {"authorized", "https", raw, 204}} {
			t.Run(path+tc.name, func(t *testing.T) {
				r := httptest.NewRequest("POST", tc.scheme+"://broker"+path+"?project=demo", strings.NewReader("x"))
				if tc.token != "" {
					r.Header.Set("Authorization", "Bearer "+tc.token)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Fatalf("status=%d want%d", w.Code, tc.want)
				}
			})
		}
	}
}
func TestAttachmentSettingStrictDefaultOff(t *testing.T) {
	s := userStore(t)
	if s.AttachmentsEnabled() {
		t.Fatal("default enabled")
	}
	if e := s.SetAttachmentPolicy("viewer", true); e == nil {
		t.Fatal("nonadmin changed switch")
	}
	if e := s.SetAttachmentPolicy("admin", true, "stale"); e == nil {
		t.Fatal("stale setting accepted")
	}
	if e := s.SetAttachmentPolicy("admin", true); e != nil {
		t.Fatal("enable")
	}
	if !s.AttachmentsEnabled() {
		t.Fatal("enable not applied")
	}
	for _, raw := range []string{`{"enabled":false,"enabled":true,"actor":"admin","at":"2026-11-04T00:00:00Z"}`, `{}`, `{"enabled":true,"actor":"admin","at":"2026-11-04T00:00:00Z","unknown":1}`, `{"enabled":true}`, `{"enabled":"true","actor":"admin","at":"2026-11-04T00:00:00Z"}`, `null`, `{"enabled":true,"actor":"admin","at":"2026-11-04T00:00:00Z"} {}`} {
		if decodeAttachmentSetting([]byte(raw)) != nil {
			t.Fatal("invalid policy enabled")
		}
	}
	if decodeAttachmentSetting([]byte(`{"enabled":true,"actor":"admin","at":"2026-11-04T00:00:00Z"}`)) == nil {
		t.Fatal("valid policy rejected")
	}
}
func TestAttachmentSettingsCheckbox(t *testing.T) {
	h := New(userStore(t), &ownedPanelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	w := admin.call("GET", "/admin/settings", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `name="attachments"`) {
		t.Fatal("attachment switch missing")
	}
}

func TestAttachmentSettingFalseHistoryAndNoop(t *testing.T) {
	s := userStore(t)
	now := time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)
	s.now = func() time.Time { return now }
	writes := 0
	write := s.writeBatch
	s.writeBatch = func(put map[string][]byte, drop []string) error { writes++; return write(put, drop) }
	if err := s.SetAttachmentPolicy("admin", false); err != nil {
		t.Fatal("default no-op")
	}
	if writes != 0 {
		t.Error("default same-value save wrote a record")
	}
	if err := s.SetAttachmentPolicy("admin", true); err != nil {
		t.Fatal("enable")
	}
	first := writes
	now = now.Add(time.Minute)
	if err := s.SetAttachmentPolicy("admin", true); err != nil {
		t.Fatal("enabled no-op")
	}
	if writes != first {
		t.Error("same-value save changed record/history")
	}
	if err := s.SetAttachmentPolicy("admin", false); err != nil {
		t.Fatal("disable")
	}
	dir := s.root.Name()
	if err := s.Close(); err != nil {
		t.Fatal("close")
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal("reopen")
	}
	defer reopened.Close()
	h := New(reopened, &ownedPanelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	page := admin.call("GET", "/admin/settings", nil)
	body := page.Body.String()
	if reopened.AttachmentsEnabled() || page.Code != 200 {
		t.Fatal("false setting not restored")
	}
	if !strings.Contains(body, `datetime="2026-11-04T01:03:03Z"`) || !strings.Contains(body, "admin") {
		t.Error("disabled setting actor/time history missing")
	}
}

func TestAttachmentSettingsSnapshot(t *testing.T) {
	s := userStore(t)
	h := New(s, &ownedPanelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if err := s.SetAttachmentPolicy("admin", i%2 == 0); err != nil {
				t.Error("policy toggle")
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		page := admin.call("GET", "/admin/settings", nil)
		body := page.Body.String()
		start := strings.Index(body, `action="/admin/attachments"`)
		if start < 0 {
			t.Fatal("form")
		}
		form := body[start:]
		end := strings.Index(form, "</form>")
		if end < 0 {
			t.Fatal("form end")
		}
		form = form[:end]
		enabled := strings.Contains(form, `name="attachments" value="false"`)
		want := policySettingsVersion("attachments", enabled)
		if !strings.Contains(form, `name="settings_version" value="`+want+`"`) {
			t.Error("flag and version were read from different snapshots")
		}
	}
	<-done
}

func TestAttachmentCardFailureFirst(t *testing.T) {
	h := New(userStore(t), &ownedPanelBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	w := admin.call("GET", "/admin/settings", nil)
	for _, want := range []string{`class="card attachment-card"`, `data-confirm-action="attachments"`, `id="attachment-free"`, `서버 디스크`, `100 MiB`, `업로드 후 30분`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("attachment card contract absent: %s", want)
		}
	}
}
