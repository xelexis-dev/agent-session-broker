package controlpanel

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestProjectTokenProfilePagesUseAccountLanguage(t *testing.T) {
	s := userStore(t)
	if _, err := s.IssueProjectTokenFor("admin", "demo", "catalog-parent"); err != nil {
		t.Fatal("create fixture token")
	}
	editor := ""
	for _, project := range s.ProjectsFor("admin") {
		if project.Name == "demo" {
			for _, token := range project.Tokens {
				if token.Label == "catalog-parent" {
					editor = token.ID
				}
			}
		}
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "admin")
	for _, language := range []string{"en", "ko"} {
		if err := s.UpdateLanguage("admin", language); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/", "/projects/settings?project=demo", "/profile", "/profile/api-tokens", "/profile/api-tokens/new", "/projects/settings?project=demo&edit=" + editor} {
			w := c.call("GET", path, nil)
			if w.Code != 200 {
				t.Errorf("%s %s: status%d", language, path, w.Code)
				continue
			}
			body := dataBlockRE.ReplaceAllString(w.Body.String(), "")
			// Language names stay in their own language in both locales.
			body = strings.ReplaceAll(body, "한국어", "")
			if language == "en" && hangulRE.MatchString(body) {
				t.Errorf("English %s still has Korean UI text", path)
			}
			if language == "ko" && !hangulRE.MatchString(body) {
				t.Errorf("Korean %s lost its translated body", path)
			}
		}
	}
}

func TestProjectPageMessagesPreserveLanguageAndUserContent(t *testing.T) {
	s := userStore(t)
	backend := &deletingBackend{}
	h := New(s, backend, http.NotFoundHandler())
	c := loginPanelUser(t, h, "admin")
	name := "이름 {n} </script>"
	if err := s.UpdateProfile("admin", name, "person@example.test"); err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"en", "ko"} {
		if err := s.UpdateLanguage("admin", language); err != nil {
			t.Fatal(err)
		}
		w := c.call("GET", "/profile", nil)
		if !strings.Contains(w.Body.String(), template.HTMLEscapeString(name)) || strings.Contains(w.Body.String(), name) {
			t.Errorf("%s: user content translated or unescaped", language)
		}
		for _, tc := range []struct {
			path, key string
			form      url.Values
		}{
			{"/profile/language", "profile.error_language", url.Values{"language": {"fr"}}},
			{"/projects/delete", "projects.error_confirm_name", url.Values{"project": {"demo"}, "confirmName": {"wrong"}}},
		} {
			w := c.call("POST", tc.path, tc.form)
			want := localText(language, tc.key)
			if w.Code != 400 || !strings.Contains(w.Body.String(), template.HTMLEscapeString(want)) {
				t.Errorf("%s %s: status%d or missing translated error", language, tc.path, w.Code)
			}
		}
	}
	if len(backend.deleted) != 0 {
		t.Fatal("invalid confirmation deleted project data")
	}
}
