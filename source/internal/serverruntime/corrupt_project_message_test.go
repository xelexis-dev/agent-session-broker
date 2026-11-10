package serverruntime

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const corruptProjectNotice = "project data unavailable: the server could not read this project's stored data. Ask the administrator to restore it from a backup."

func TestCorruptProjectMCPExplainsRecovery(t *testing.T) {
	root, _, bad := r2CorruptRegistrationFixture(t)
	handler, closeHandler, err := newManagedHandler(root, nil)
	if err != nil {
		t.Fatal("isolated corruption blocked startup")
	}
	defer closeHandler()
	req := httptest.NewRequest("POST", "https://broker/mcp?project=bad", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+bad)
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != 503 || strings.TrimSpace(out.Body.String()) != corruptProjectNotice {
		t.Fatal("corrupt project MCP response lacks the fixed backup recovery notice")
	}
	// Authorization still precedes storage diagnostics.
	req.Header.Del("Authorization")
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != 401 || strings.Contains(out.Body.String(), corruptProjectNotice) {
		t.Fatal("unauthorized caller learned storage state")
	}
}

func TestCorruptProjectAdminPagesExplainRecovery(t *testing.T) {
	root, _, _ := r2CorruptRegistrationFixture(t)
	handler, closeHandler, err := newManagedHandler(root, nil)
	if err != nil {
		t.Fatal("fixture startup failed")
	}
	defer closeHandler()
	r := &deviceRuntime{t: t, h: handler}
	r.login("synthetic-round2-password")
	for _, page := range []string{"settings", "sessions", "audit"} {
		out := r.browser(http.MethodGet, "/projects/"+page+"?project=bad", nil)
		if out.Code != 200 || !strings.Contains(out.Body.String(), `class="notice error" role="alert">이 프로젝트의 저장 데이터를 읽을 수 없습니다. 관리자가 백업에서 복원해야 합니다.</div>`) || strings.Contains(out.Body.String(), "project data unavailable:") {
			t.Errorf("%s lacks the existing error notice with backup recovery guidance", page)
		}
		if bytes.Contains(out.Body.Bytes(), []byte(root)) || strings.Contains(out.Body.String(), "synthetic damaged SQLite header") {
			t.Fatal("notice leaks storage details")
		}
	}
	out := r.browser(http.MethodGet, "/projects/settings?project=test", nil)
	if out.Code != 200 || strings.Contains(out.Body.String(), "이 프로젝트의 저장 데이터를 읽을 수 없습니다.") {
		t.Fatal("healthy project inherited corrupt state")
	}
}

// Optional visual check against the real isolated managed handler, never a shared server.
func TestCorruptProjectFullChrome(t *testing.T) {
	script := os.Getenv("ASB_CORRUPT_BROWSER_SCRIPT")
	if script == "" {
		t.Skip("opt-in full Chrome")
	}
	root, _, _ := r2CorruptRegistrationFixture(t)
	handler, closeHandler, err := newManagedHandler(root, nil)
	if err != nil {
		t.Fatal("fixture startup failed")
	}
	defer closeHandler()
	r := &deviceRuntime{t: t, h: handler}
	r.login("synthetic-round2-password")
	server := httptest.NewServer(handler)
	defer server.Close()
	config := struct{ Base, CookieName, Cookie string }{server.URL, r.cookie.Name, r.cookie.Value}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal("private browser config failed")
	}
	private := filepath.Join(t.TempDir(), "browser-private.json")
	if os.WriteFile(private, data, 0600) != nil {
		t.Fatal("private browser config failed")
	}
	cmd := exec.CommandContext(t.Context(), "node", script, private)
	if cmd.Run() != nil {
		t.Fatal("full Chrome failed; inspect sanitized evidence status")
	}
}
