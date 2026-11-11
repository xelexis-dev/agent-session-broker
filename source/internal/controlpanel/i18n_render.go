package controlpanel

import (
	"html/template"
	"net/http"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

// pageSets holds one parsed template set per language. Each set binds {{t "key"}}, {{tm message}}, {{lang}} and
// {{jsMessages}} to its language, so a nested template translates correctly whatever its dot is.
var pageSets = parsePageSets()

// pages is the English set: the default, and what tests that execute templates directly render.
var pages = pageSets[i18n.English]

func parsePageSets() map[string]*template.Template {
	sets := map[string]*template.Template{}
	for _, language := range i18n.Languages {
		language := language
		sets[language] = template.Must(template.New("templates.html").Funcs(template.FuncMap{
			"asset": assetURL,
			"scopeCodes": func(scopes []string) string {
				if len(scopes) == 0 {
					return i18n.T(language, "tokens.none")
				}
				return codesText(scopes)
			},
			"t":          func(key string, args ...any) string { return i18n.T(language, key, args...) },
			"tm":         func(m i18n.Message) string { return m.In(language) },
			"lang":       func() string { return language },
			"jsMessages": func() map[string]any { return i18n.JS(language) },
		}).ParseFS(assets, "admin-api-panel.html", "templates.html", "direct-audit.html", "bug-reports.html", "token-scope-editor.html"))
	}
	return sets
}

// requestLanguage is the account setting when the request is signed in, else the asb_lang cookie copied from the
// last account on this browser (QA71 2B), else English. It never decides authorization.
func (p *panel) requestLanguage(r *http.Request, userID string) string {
	if userID == "" && r != nil {
		if session, ok := p.session(r); ok {
			userID = session.userID
		}
	}
	if userID != "" {
		return p.store.UserLanguage(userID)
	}
	if r != nil {
		if cookie, err := r.Cookie(languageCookie); err == nil && i18n.Normalize(cookie.Value) != "" {
			return cookie.Value
		}
	}
	return i18n.English
}

// localText resolves a catalog key at the request boundary; non-key data passes through unchanged.
func localText(language, text string) string {
	if i18n.Has(text) {
		return i18n.T(language, text)
	}
	return text
}
