package controlpanel

import (
	"net/http"
	"strings"
	"testing"
)

func TestSidebarVersionOnPanelPagesAndEscaped(t *testing.T) {
	for _, version := range []string{"9.9.9-test", "", "  ", `<script>alert("version")</script>`} {
		t.Run(version, func(t *testing.T) {
			s := userStore(t)
			h := New(s, tokenViewBackend{}, http.NotFoundHandler(), version)
			client := loginPanelUser(t, h, "admin")
			expected := strings.TrimSpace(version)
			if expected == "" {
				expected = "dev"
			}
			for _, path := range []string{"/", "/profile", "/projects/sessions?project=demo", "/projects/settings?project=demo", "/auth/code", "/admin/users", "/admin/settings"} {
				w := client.call("GET", path, nil)
				if w.Code != 200 {
					t.Fatalf("page unavailable: %s", path)
				}
				body := w.Body.String()
				start := strings.Index(body, `<aside class="sidebar"`)
				end := strings.Index(body, `</aside>`)
				if start < 0 || end <= start {
					t.Fatal("sidebar missing")
				}
				sidebar := body[start:end]
				if strings.Contains(expected, "<script>") {
					if strings.Contains(sidebar, "<script>") || !strings.Contains(sidebar, "&lt;script&gt;") {
						t.Fatal("version not HTML escaped")
					}
				} else if !strings.Contains(sidebar, `class="sidebar__version">Version `+expected+`</p>`) {
					t.Fatalf("version missing: %s", path)
				}
			}
		})
	}
}
