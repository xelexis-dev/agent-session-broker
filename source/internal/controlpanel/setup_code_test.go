package controlpanel

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestSetupRejectsMissingAndWrongCode(t *testing.T) {
	for _, code := range []string{"", "incorrect-setup-proof"} {
		t.Run(map[bool]string{true: "missing", false: "wrong"}[code == ""], func(t *testing.T) {
			s, err := OpenSQLite(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := New(s, &panelBackend{}, http.NotFoundHandler())
			page := httptest.NewRecorder()
			h.ServeHTTP(page, httptest.NewRequest("GET", "http://broker/setup", nil))
			csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
			if len(csrf) != 2 {
				t.Fatal("CSRF missing")
			}
			values := url.Values{"csrf": {csrf[1]}, "setupCode": {code}, "password": {testPassword}, "confirmPassword": {testPassword}}
			req := httptest.NewRequest("POST", "http://broker/setup", strings.NewReader(values.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(page.Result().Cookies()[0])
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 403 || s.Ready() {
				t.Fatalf("setup refusal: status=%d admin=%t", w.Code, s.Ready())
			}
			if code != "" && strings.Contains(w.Body.String(), code) {
				t.Fatal("setup response exposed input")
			}
		})
	}
}

func setupCodeForTest(t *testing.T, s *Store) string {
	t.Helper()
	f, err := s.root.Open("setup-code")
	if err != nil {
		t.Fatal("setup proof file unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatal("setup proof read failed")
	}
	return strings.TrimSpace(string(raw))
}

func TestSetupCodeLifecycleAndFailures(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	code := setupCodeForTest(t, s)
	if !regexp.MustCompile(`^[A-Z2-7]{26}$`).MatchString(code) {
		t.Fatal("invalid setup proof format")
	}
	info, err := os.Stat(filepath.Join(dir, "setup-code"))
	if err != nil {
		t.Fatal("proof stat failed")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("proof is not private")
	}
	for _, password := range []string{"short", strings.Repeat("x", 1025)} {
		if !errors.Is(s.SetupWithCode(code, password), ErrInvalid) || s.Ready() {
			t.Fatal("invalid password committed")
		}
	}
	write := s.write
	s.write = func([]byte) error { return ErrStorage }
	if !errors.Is(s.SetupWithCode(code, testPassword), ErrStorage) || s.Ready() {
		t.Fatal("failed save committed")
	}
	s.write = write
	if setupCodeForTest(t, s) != code {
		t.Fatal("failed setup consumed proof")
	}
	s.Close()
	// Model crash immediately after O_EXCL creation and after the complete write.
	for _, partial := range []string{"", "incomplete synthetic proof\n"} {
		if os.WriteFile(filepath.Join(dir, "setup-code.tmp"), []byte(partial), 0600) != nil {
			t.Fatal("stale temp fixture")
		}
		s, err = OpenSQLite(dir)
		if err != nil {
			t.Fatal("stale temp did not recover")
		}
		next := setupCodeForTest(t, s)
		if next == code {
			t.Fatal("restart reused proof")
		}
		if !errors.Is(s.SetupWithCode(code, testPassword), ErrSetupCode) || s.Ready() {
			t.Fatal("old proof accepted")
		}
		code = next
		s.Close()
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	code = setupCodeForTest(t, s)
	if err = s.SetupWithCode(" \n"+strings.ToLower(code)+"\n ", testPassword); err != nil {
		t.Fatal("normalized proof refused")
	}
	if !s.Ready() || !s.Authenticate(testPassword) {
		t.Fatal("setup did not commit")
	}
	if _, err = os.Stat(filepath.Join(dir, "setup-code")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("proof file survived setup")
	}
	if !errors.Is(s.SetupWithCode(code, testPassword), ErrAlreadySetup) {
		t.Fatal("already setup check lost")
	}
	s.Close()
	if os.Mkdir(filepath.Join(dir, "setup-code"), 0700) != nil {
		t.Fatal("stale directory fixture")
	}
	if os.WriteFile(filepath.Join(dir, "setup-code", "keep"), nil, 0600) != nil {
		t.Fatal("stale directory contents fixture")
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal("configured store failed on stale proof")
	}
	defer s.Close()
	if !s.Authenticate(testPassword) {
		t.Fatal("reopen changed credentials")
	}
}

func TestSetupCodeUnexpectedObjectsFailClosed(t *testing.T) {
	for _, name := range []string{"setup-code", "setup-code.tmp"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if os.Mkdir(filepath.Join(dir, name), 0700) != nil {
				t.Fatal("directory fixture")
			}
			s, err := OpenSQLite(dir)
			if s != nil {
				s.Close()
			}
			if !errors.Is(err, ErrStorage) {
				t.Fatal("unexpected object accepted")
			}
		})
	}
}

func TestSetupCodeConcurrentHTTPAndAttemptLimit(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	form := httptest.NewRecorder()
	h.ServeHTTP(form, httptest.NewRequest("GET", "http://broker/setup", nil))
	html := form.Body.String()
	for _, attr := range []string{`name="setupCode"`, `autocomplete="one-time-code"`, `aria-describedby="setup-code-help"`, `id="setup-code-help"`} {
		if !strings.Contains(html, attr) {
			t.Fatal("proof input/help missing")
		}
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(html)[1]
	post := func(code string) int {
		values := url.Values{"csrf": {csrf}, "setupCode": {code}, "password": {testPassword}, "confirmPassword": {testPassword}}
		r := httptest.NewRequest("POST", "http://broker/setup", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(form.Result().Cookies()[0])
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 10; i++ {
		if post("wrong") != 403 {
			t.Fatal("invalid code not refused")
		}
	}
	if post("wrong") != 429 {
		t.Fatal("attempt limit lost")
	}
	// A fresh panel has its own limiter; store/code ownership is unchanged.
	h = New(s, &panelBackend{}, http.NotFoundHandler())
	code := setupCodeForTest(t, s)
	start := make(chan struct{})
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- post(code) }()
	}
	close(start)
	a, b := <-results, <-results
	if !(a == 303 && b == 409 || a == 409 && b == 303) {
		t.Fatalf("concurrent setup statuses=%d,%d", a, b)
	}
}
