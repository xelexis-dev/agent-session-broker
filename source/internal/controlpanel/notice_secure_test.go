package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
)

func TestNoticeCookiesSecureBehindTrustedTLSProxy(t *testing.T) {
	s := userStore(t)
	inner := NewWithTrustedProxies(s, tokenViewBackend{}, http.NotFoundHandler(), []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Forwarded-Proto", "https")
		inner.ServeHTTP(w, r)
	})
	admin := loginPanelUser(t, h, "admin")
	saved := admin.call("POST", "/profile/save", url.Values{"displayName": {"Changed"}, "email": {"changed@example.test"}})
	if saved.Code != 303 {
		t.Fatal("save did not redirect")
	}
	var notice *http.Cookie
	for _, c := range saved.Result().Cookies() {
		if c.Name == "asb_notice" {
			notice = c
		}
	}
	if notice == nil {
		t.Fatal("notice cookie missing")
	}
	if !notice.Secure || !notice.HttpOnly || notice.SameSite != http.SameSiteStrictMode {
		t.Error("notice cookie must be Secure/HttpOnly/Strict behind trusted TLS proxy")
	}
	get := httptest.NewRequest("GET", "http://broker/profile", nil)
	get.AddCookie(admin.cookie)
	get.AddCookie(notice)
	consumed := httptest.NewRecorder()
	h.ServeHTTP(consumed, get)
	found := false
	for _, c := range consumed.Result().Cookies() {
		if c.Name == "asb_notice" {
			found = true
			if !c.Secure || c.MaxAge != -1 {
				t.Error("notice deletion cookie must be Secure")
			}
		}
	}
	if !found {
		t.Fatal("notice deletion cookie missing")
	}
	changed := admin.call("POST", "/profile/password", url.Values{"currentPassword": {testPassword}, "newPassword": {"next test password 220"}, "confirmPassword": {"next test password 220"}})
	if changed.Code != 303 {
		t.Fatal("password change did not redirect")
	}
	found = false
	for _, c := range changed.Result().Cookies() {
		if c.Name == "asb_admin" {
			found = true
			if !c.Secure || c.MaxAge != -1 {
				t.Error("profile login deletion cookie must be Secure")
			}
		}
	}
	if !found {
		t.Fatal("profile login deletion cookie missing")
	}
}

func TestNoticeCookieTransportBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, scheme, peer, forwarded string
		secure                        bool
	}{
		{"plain", "http", "198.51.100.1:1234", "", false},
		{"direct-TLS", "https", "198.51.100.1:1234", "", true},
		{"untrusted-forwarded", "http", "198.51.100.1:1234", "https", false},
		{"trusted-TLS", "http", "127.0.0.1:1234", "https", true},
		{"trusted-plaintext-over-TLS", "https", "127.0.0.1:1234", "http", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &panel{trustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
			r := httptest.NewRequest("GET", tc.scheme+"://broker/profile", nil)
			r.RemoteAddr = tc.peer
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			set := httptest.NewRecorder()
			p.redirectNotice(set, r, "/profile", "profile-saved")
			cookies := set.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Secure != tc.secure {
				t.Fatal("notice set transport policy mismatch")
			}
			r.AddCookie(cookies[0])
			clear := httptest.NewRecorder()
			if p.takeNotice(clear, r) == "" {
				t.Fatal("notice not consumed")
			}
			cookies = clear.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Secure != tc.secure || cookies[0].MaxAge != -1 {
				t.Fatal("notice clear transport policy mismatch")
			}
		})
	}
}
