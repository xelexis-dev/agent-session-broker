package controlpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAttachmentUIFullChrome(t *testing.T) {
	if os.Getenv("ASB_ATTACHMENT_UI_BROWSER") == "" {
		t.Skip("opt-in synthetic attachment UI full Chrome")
	}
	type row struct{ State, Base, CookieName, Cookie string }
	var rows []row
	var hideTitle atomic.Bool
	for _, state := range []string{"off", "on", "low", "unknown", "long", "off-low", "off-unknown"} {
		s := attachmentUIFixture(t, state)
		h := New(s, &ownedPanelBackend{}, http.NotFoundHandler())
		admin := loginPanelUser(t, h, "admin")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/browser-hidden-status.css" {
				w.Header().Set("Content-Type", "text/css")
				_, _ = w.Write([]byte("#attachment-status-title{display:none}"))
				return
			}
			if !hideTitle.Load() || r.URL.Path != "/admin/settings" {
				h.ServeHTTP(w, r)
				return
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			for key, values := range response.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write([]byte(strings.Replace(response.Body.String(), "</head>", `<link rel="stylesheet" href="/browser-hidden-status.css"></head>`, 1)))
		}))
		defer server.Close()
		rows = append(rows, row{state, server.URL, admin.cookie.Name, admin.cookie.Value})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal("browser config")
	}
	file := filepath.Join(t.TempDir(), "browser-private.json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal("browser config write")
	}
	if err = exec.CommandContext(t.Context(), "node", "attachment_ui_browser.cjs", file).Run(); err != nil {
		t.Fatal("attachment browser assertions failed; inspect sanitized evidence")
	}
	t.Run("reject_hidden_status", func(t *testing.T) {
		evidence := t.TempDir()
		t.Setenv("ASB_ATTACHMENT_UI_EVIDENCE", evidence)
		hideTitle.Store(true)
		if err := exec.CommandContext(t.Context(), "node", "attachment_ui_browser.cjs", file).Run(); err == nil {
			t.Fatal("browser accepted hidden attachment status copy")
		}
		raw, err := os.ReadFile(filepath.Join(evidence, "failure.json"))
		var failure struct{ Kind, Reason string }
		if err != nil || json.Unmarshal(raw, &failure) != nil || failure.Kind != "AssertionError" || failure.Reason != "attachment status title is visible" {
			t.Fatal("hidden-status mutation did not fail the visibility assertion")
		}
	})

}
