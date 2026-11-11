package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// QA71 2B: a page without an account uses the asb_lang cookie copied from the last account, else English.
func TestPreLoginPagesFollowTheLanguageCookie(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	for _, tc := range []struct{ cookie, lang string }{{"", "en"}, {"ko", "ko"}, {"en", "en"}, {"fr", "en"}, {"<ko>", "en"}} {
		r := httptest.NewRequest("GET", "http://broker/login", nil)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: languageCookie, Value: tc.cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, `<html lang="`+tc.lang+`">`) {
			t.Errorf("cookie %q: status %d, want lang %q", tc.cookie, w.Code, tc.lang)
		}
	}
}

var (
	dataBlockRE   = regexp.MustCompile(`(?s)<script type="application/json" id="asb-i18n">(.*?)</script>`)
	placeholderRE = regexp.MustCompile(`\{[a-z][a-z0-9_]*\}`)
	hangulRE      = regexp.MustCompile(`\p{Hangul}`)
)

// Every panel view, including every admin view and pre-login view, is covered in both languages.
func TestEveryViewRendersInTheAccountLanguage(t *testing.T) {
	runPanelViewLanguageMatrix(t)
}

// The live endpoint answers in the polling page's language: one lang=en|ko, anything else means a pre-1.1 (Korean)
// tab. The parameter never changes access.
func TestLiveSessionLanguageFollowsThePage(t *testing.T) {
	for query, want := range map[string]string{"": "ko", "lang=en": "en", "lang=ko": "ko", "lang=fr": "ko", "lang=en&lang=ko": "ko", "lang=": "ko"} {
		if got := liveLanguage(httptest.NewRequest("GET", "http://broker/projects/sessions/live?project=demo&"+query, nil)); got != want {
			t.Errorf("%q: %s, want %s", query, got, want)
		}
	}
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/projects/sessions/live?project=demo&lang=en", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated live request with lang: %d", w.Code)
	}
	outsider := loginPanelUser(t, h, "outsider").call("GET", "/projects/sessions/live?project=demo&lang=en", nil)
	if outsider.Code != http.StatusForbidden {
		t.Errorf("non-member live request with lang: %d", outsider.Code)
	}
}
