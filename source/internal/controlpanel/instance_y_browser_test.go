package controlpanel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type instanceYBackend struct{ panelBackend }

func (b *instanceYBackend) Announce(_ context.Context, _, _, _ string) ([]AnnouncementResult, error) {
	var rows []AnnouncementResult
	for i := 0; i < 12; i++ {
		row := AnnouncementResult{Project: "acc", Name: fmt.Sprintf("worker-%02d", i+1), Accepted: true}
		if i%2 == 1 {
			row.Project = "asb"
		}
		if i == 2 {
			row.Project = "customer-experience-platform-observability-production-system"
			row.Name = "agent-" + strings.Repeat("x", 74)
		}
		if i == 3 {
			row.Accepted = false
			row.Error = "대기 큐가 가득 찼습니다."
		}
		if i == 9 {
			row.Accepted = false
			row.Error = "수신 준비 안 됨"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func TestInstanceYBrowser(t *testing.T) {
	script := os.Getenv("ASB_INSTANCE_Y_BROWSER")
	if script == "" {
		t.Skip("opt-in design fixture")
	}
	bases := map[string]string{}
	for _, state := range []string{"on", "off", "results", "error"} {
		s, err := OpenSQLite(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.Setup(testPassword); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"acc", "asb", "customer-experience-platform-observability-production-system"} {
			if err := s.CreateProjectFor("admin", name); err != nil {
				t.Fatal(err)
			}
		}
		if state != "off" {
			if err := s.SetScopeIssuanceReady("admin", true); err != nil {
				t.Fatal(err)
			}
		}
		handler := New(s, &instanceYBackend{}, http.NotFoundHandler())
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/fixture/message" || r.URL.Path == "/fixture/empty" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				v := page{Form: url.Values{}}
				if r.URL.Path == "/fixture/message" {
					v.AnnouncementMessage = "공지 접수 처리 중입니다."
				} else {
					v.AnnouncementComplete = true
				}
				if err := pages.ExecuteTemplate(w, "admin-settings", v); err != nil {
					t.Error("fixture template render failed")
				}
				return
			}
			handler.ServeHTTP(w, r)
		}))
		defer server.Close()
		bases[state] = server.URL
	}
	data, err := json.Marshal(map[string]any{"bases": bases, "password": testPassword})
	if err != nil {
		t.Fatal("fixture config")
	}
	config := filepath.Join(t.TempDir(), "browser.json")
	if os.WriteFile(config, data, 0600) != nil {
		t.Fatal("fixture write")
	}
	if _, err := exec.CommandContext(t.Context(), "node", script, config).CombinedOutput(); err != nil {
		t.Fatal("instance Y browser failed; inspect evidence status.json")
	}
}
