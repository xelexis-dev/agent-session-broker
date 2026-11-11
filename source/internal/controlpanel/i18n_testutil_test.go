package controlpanel

import (
	"html/template"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

// en is the English catalog text that a default (English) account sees; tests assert it instead of a copied literal.
func en(key string, args ...any) string { return i18n.T(i18n.English, key, args...) }

// enHTML is en as html/template writes it into a page (for example ' becomes &#39;).
func enHTML(key string, args ...any) string { return template.HTMLEscapeString(en(key, args...)) }

// koPages is the Korean template set, for fixtures that render a template directly.
var koPages = pageSets[i18n.Korean]

// useKorean sets every account of s to Korean. Browser tests whose page bodies are still Korean use it so their
// assertions keep covering the Korean path; MR-3/4/5 move them to English with their page group.
func useKorean(t *testing.T, s *Store) {
	t.Helper()
	s.mu.RLock()
	ids := make([]string, 0, len(s.data.Users))
	for id := range s.data.Users {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		if err := s.UpdateLanguage(id, "ko"); err != nil {
			t.Fatal(err)
		}
	}
}

func ko(key string, args ...any) string { return i18n.T(i18n.Korean, key, args...) }
