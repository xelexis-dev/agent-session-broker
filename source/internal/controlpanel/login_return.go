package controlpanel

import (
	"net/http"
	"net/url"
	"strings"
)

// Only HTML GET views can be resumed. APIs and state-changing endpoints must
// not be replayed after authentication, even when their paths look local.
func safeLoginNext(raw string) string {
	if len(raw) > 4096 || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, " ") || unsafeLoginCharacters(raw) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || strings.Contains(raw, "#") {
		return ""
	}
	// Every allowed path is plain ASCII. Reject encoded paths rather than
	// decoding/cleaning them into a different route (including double encoding).
	if u.EscapedPath() != u.Path || strings.Contains(u.Path, "%") {
		return ""
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ""
	}
	for key, values := range query {
		if unsafeLoginCharacters(key) {
			return ""
		}
		for _, value := range values {
			if unsafeLoginCharacters(value) {
				return ""
			}
		}
	}
	switch u.Path {
	case "/", "/projects/settings", "/projects/sessions", "/projects/audit", "/admin/users", "/admin/users/detail", "/admin/settings", "/profile", deviceVerificationPath, "/admin/announcement/result":
		return raw
	default:
		return ""
	}
}

func unsafeLoginCharacters(s string) bool {
	for _, r := range s {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func loginLocation(r *http.Request) string {
	if r.Method == http.MethodGet {
		if next := safeLoginNext(r.URL.RequestURI()); next != "" {
			return "/login?next=" + url.QueryEscape(next)
		}
	}
	return "/login"
}

func loginNext(r *http.Request) string {
	values := r.URL.Query()["next"]
	if r.Method == http.MethodPost {
		values = r.PostForm["next"]
	}
	if len(values) != 1 {
		return ""
	}
	return safeLoginNext(values[0])
}
