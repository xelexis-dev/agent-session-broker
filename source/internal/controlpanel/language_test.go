package controlpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLanguagePATCHAndRead(t *testing.T) {
	s, h := apiFixture(t)
	token, _ := apiIssue(t, s, "read", "write")
	w := apiRequest(h, "PATCH", "/api/v1/me", `{"language":"ko"}`, token)
	if w.Code != 200 {
		t.Fatalf("language PATCH status=%d; want 200", w.Code)
	}
	w = apiRequest(h, "GET", "/api/v1/me", "", token)
	var v map[string]any
	if json.Unmarshal(w.Body.Bytes(), &v) != nil || v["language"] != "ko" {
		t.Fatal("saved language missing from self view")
	}
}

func TestLanguageAtomicFailureAndDigest(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(fmt.Sprint(prune), func(t *testing.T) {
			s, h := apiFixture(t)
			token, _ := apiIssue(t, s, "read", "write")
			before := apiRequest(h, "GET", "/api/v1/me", "", token)
			var view map[string]any
			json.Unmarshal(before.Body.Bytes(), &view)
			priorUser, _ := s.User("admin")
			priorAudits := len(s.api.audits)
			if prune {
				s.audits["audit/orphan"] = AuditEvent{Project: "gone", ProjectGenerationID: "synthetic-generation"}
			}
			original := s.writeBatch
			calls := 0
			s.writeBatch = func(puts map[string][]byte, deletes []string) error {
				calls++
				if puts["state"] == nil || puts["user-settings/admin"] == nil {
					t.Fatal("state/language missing from atomic batch")
				}
				audit := false
				for key, raw := range puts {
					if strings.HasPrefix(key, "api-audit/") && bytes.Contains(raw, []byte(`"profile_update"`)) {
						audit = true
					}
				}
				if !audit {
					t.Fatal("profile audit missing from atomic batch")
				}
				return errors.New("synthetic failed transaction")
			}
			w := apiRequest(h, "PATCH", "/api/v1/me", `{"display_name":"changed","language":"ko"}`, token)
			s.writeBatch = original
			if w.Code != 503 || calls != 1 || s.UserLanguage("admin") != "en" || len(s.api.audits) != priorAudits {
				t.Fatal("failed transaction published language/audit")
			}
			after, _ := s.User("admin")
			if after != priorUser {
				t.Fatal("failed transaction changed profile")
			}
			records, _ := s.listRecords()
			if records["user-settings/admin"] != nil {
				t.Fatal("failed transaction left language row")
			}
			w = apiRequest(h, "PATCH", "/api/v1/me", `{"display_name":"changed","language":"ko"}`, token)
			if w.Code != 200 || s.UserLanguage("admin") != "ko" {
				t.Fatal("combined update failed")
			}
			stale, _ := json.Marshal(map[string]any{"display_name": "stale", "version": view["version"]})
			if w = apiRequest(h, "PATCH", "/api/v1/me", string(stale), token); w.Code != 409 {
				t.Fatal("language change did not invalidate profile digest")
			}
			if w = apiRequest(h, "PATCH", "/api/v1/me", `{"language":"en"}`, token); w.Code != 200 {
				t.Fatal("versionless language-only change failed")
			}
			after, _ = s.User("admin")
			if after.Revision != priorUser.Revision {
				t.Fatal("language invalidated credentials")
			}
			for _, body := range []string{`{"language":null}`, `{"language":""}`, `{"language":"ja"}`, `{"language":false}`, `{"language":["ko"]}`} {
				if w = apiRequest(h, "PATCH", "/api/v1/me", body, token); w.Code != 400 {
					t.Fatalf("invalid language accepted, status=%d", w.Code)
				}
			}
			records, _ = s.listRecords()
			if bytes.Contains(records["state"], []byte(`"language"`)) {
				t.Fatal("language leaked into persisted state")
			}
		})
	}
}

func TestLanguageLoadingAndPreservation(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if s.UserLanguage("admin") != "en" {
		t.Fatal("unset not English")
	}
	if err = s.UpdateLanguage("admin", "ko"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.UserLanguage("admin") != "ko" {
		t.Fatal("language not persisted")
	}
	s.Close()
	for _, raw := range []string{`{`, `{"v":2,"language":"ko"}`, `{"v":1,"language":"ja"}`, `{"v":1,"language":false}`, strings.Repeat(" ", 1025)} {
		db, err := sqlitestore.Open(dir, "control.db")
		if err != nil {
			t.Fatal(err)
		}
		if err = db.Put("user-settings/admin", []byte(raw)); err != nil {
			t.Fatal(err)
		}
		db.Close()
		s, err = OpenSQLite(dir)
		if err != nil {
			t.Fatal("bad preference prevented startup")
		}
		if s.UserLanguage("admin") != "en" || s.invalidUserSettings != 1 {
			t.Fatal("bad row not counted/defaulted")
		}
		records, _ := s.listRecords()
		if string(records["user-settings/admin"]) != raw {
			t.Fatal("bad row was rewritten")
		}
		s.Close()
	}
	db, err := sqlitestore.Open(dir, "control.db")
	if err != nil {
		t.Fatal(err)
	}
	db.Put("user-settings/admin", []byte(`{"v":1,"language":"ko","future":true}`))
	db.Close()
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.UserLanguage("admin") != "ko" {
		t.Fatal("forward-compatible field rejected")
	}
	original := s.writeBatch
	s.writeBatch = func(map[string][]byte, []string) error { return errors.New("synthetic") }
	if err = s.UpdateLanguage("admin", "en"); !errors.Is(err, ErrStorage) || s.UserLanguage("admin") != "ko" {
		t.Fatal("failed browser preference changed memory")
	}
	s.writeBatch = original
	if err = s.UpdateLanguage("missing", "ko"); !errors.Is(err, ErrForbidden) {
		t.Fatal("unknown account saved language")
	}
}

func TestLanguageBrowserCookiesAndBoundary(t *testing.T) {
	s := userStore(t)
	if err := s.UpdateProfile("viewer", "Saved display", "viewer@example.test"); err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "viewer")
	for _, path := range []string{"/profile/language?language=ko", "/profile/language"} {
		w := c.call("POST", path, url.Values{"language": {"ko"}, "role": {"admin"}})
		if w.Code != 403 {
			t.Fatal("extra input accepted")
		}
	}
	bad := c.call("POST", "/profile/language", url.Values{"language": {"ja"}})
	if !strings.Contains(bad.Body.String(), `value="Saved display"`) || !strings.Contains(bad.Body.String(), `value="viewer@example.test"`) {
		t.Fatal("language failure cleared unrelated profile form")
	}
	for _, values := range []url.Values{{"language": {"ja"}}, {"language": {"ko", "en"}}, {}} {
		if w := c.call("POST", "/profile/language", values); w.Code == 303 {
			t.Fatal("invalid language saved")
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://broker/profile/language", strings.NewReader("language=ko"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(c.cookie)
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("language save missing CSRF check")
	}
	w = c.call("POST", "/profile/language", url.Values{"language": {"ko"}})
	if w.Code != 303 || s.UserLanguage("viewer") != "ko" {
		t.Fatal("browser language save failed")
	}
	checkCookie := func(w *httptest.ResponseRecorder, secure bool) {
		t.Helper()
		found := false
		for _, v := range w.Result().Cookies() {
			if v.Name == languageCookie {
				found = true
				if v.Value != "ko" || v.Path != "/" || !v.HttpOnly || v.Secure != secure || v.SameSite != http.SameSiteLaxMode || v.MaxAge != 365*24*3600 {
					t.Fatal("invalid language cookie policy")
				}
			}
		}
		if !found {
			t.Fatal("language cookie missing")
		}
	}
	checkCookie(w, false)
	p := &panel{}
	w = httptest.NewRecorder()
	p.setLanguageCookie(w, httptest.NewRequest("GET", "https://broker/profile", nil), "ko")
	checkCookie(w, true)
	// A successful login refreshes the preference cookie, not just the browser save.
	loginClient := &userPanelClient{t: t, h: h}
	form := loginClient.call("GET", "/login", nil)
	for _, cookie := range form.Result().Cookies() {
		if cookie.Name == "asb_form" {
			loginClient.cookie = cookie
			loginClient.csrf = cookie.Value
		}
	}
	login := loginClient.call("POST", "/login", url.Values{"username": {"viewer"}, "password": {testPassword}})
	if login.Code != 303 {
		t.Fatal("language login failed")
	}
	checkCookie(login, false)
	profile := c.call("GET", "/profile", nil).Body.String()
	if !strings.Contains(profile, `<option value="ko" lang="ko" selected>한국어</option>`) || !strings.Contains(profile, `data-select`) {
		t.Fatal("saved selector missing")
	}
}

func TestLanguageCrashCommit(t *testing.T) {
	if dir := os.Getenv("ASB_LANGUAGE_CRASH_DIR"); dir != "" {
		s, err := OpenSQLite(dir)
		if err != nil {
			os.Exit(2)
		}
		token, _ := apiIssue(t, s, "read", "write")
		h := New(s, nil, http.NotFoundHandler())
		original := s.writeBatch
		s.writeBatch = func(puts map[string][]byte, deletes []string) error {
			if err := original(puts, deletes); err != nil {
				os.Exit(3)
			}
			if puts[userSettingsPrefix+"admin"] != nil {
				os.Exit(0)
			}
			return nil
		}
		apiRequest(h, "PATCH", "/api/v1/me", `{"display_name":"after-crash","language":"ko"}`, token)
		os.Exit(4)
	}
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLanguageCrashCommit$")
	cmd.Env = append(os.Environ(), "ASB_LANGUAGE_CRASH_DIR="+dir)
	if _, err = cmd.CombinedOutput(); err != nil {
		t.Fatal("synthetic crash child did not commit")
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _ := s.User("admin")
	audited := false
	for _, e := range s.api.audits {
		if e.Action == "profile_update" {
			audited = true
		}
	}
	if u.DisplayName != "after-crash" || s.UserLanguage("admin") != "ko" || !audited {
		t.Fatal("restart observed partial profile commit")
	}
}

func TestLanguageAdminViewIsReadOnly(t *testing.T) {
	s, h := apiFixture(t)
	token, _ := apiIssue(t, s, "admin", "read", "write")
	if err := s.UpdateLanguage("admin", "ko"); err != nil {
		t.Fatal(err)
	}
	w := apiRequest(h, "GET", "/api/v1/admin/users/admin", "", token)
	var v map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v["language"] != "ko" {
		t.Fatal("admin user projection missing language")
	}
	w = apiRequest(h, "PATCH", "/api/v1/admin/users/admin", `{"language":"en"}`, token)
	if w.Code != 400 || s.UserLanguage("admin") != "ko" {
		t.Fatal("admin endpoint mutated display preference")
	}
}
