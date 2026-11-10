package controlpanel

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Reproduce REMOTE-MCP-MEASURE M4 with real disposable credentials, extending
// its unmarked-HTTP check to the approved device-token issuance response.
func TestTransportPolicyUnmarkedPlaintext(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, allow := range []bool{false, true} {
			for _, path := range []string{"/mcp", "/oauth/device_authorization", "/oauth/token", "/oauth/session_registration"} {
				t.Run(path+map[bool]string{false: "/OFF", true: "/ON"}[allow]+map[bool]string{false: "/plaintext", true: "/TLS"}[secure], func(t *testing.T) {
					s := directStore(t)
					if allow {
						if err := s.SetInstanceHTTPPolicy("admin", true); err != nil {
							t.Fatal(err)
						}
					}
					parent := directParent(t, s, 8)
					credential := parent
					form := directForm()
					if path == "/mcp" {
						var err error
						credential, err = s.IssueToken("demo", "fixture")
						if err != nil {
							t.Fatal(err)
						}
					}
					if path == "/oauth/token" {
						issued, err := s.BeginDeviceAuthorization("demo", parent, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", InstanceID: "fixture-client", RequestID: "fixture-device"})
						if err != nil {
							t.Fatal(err)
						}
						if _, err := s.ApproveDeviceAuthorization("admin", issued.UserCode, "device-worker", false); err != nil {
							t.Fatal(err)
						}
						form = tokenForm(issued.DeviceCode)
					}
					h := New(s, tokenViewBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
					req := httptest.NewRequest(http.MethodPost, path+"?project=demo", strings.NewReader(form.Encode()))
					req.RemoteAddr = "192.0.2.1:3456"
					if secure {
						req.TLS = &tls.ConnectionState{}
					}
					req.Header.Set("Authorization", "Bearer "+credential)
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					if !allow && !secure {
						var body map[string]string
						_ = json.Unmarshal(w.Body.Bytes(), &body)
						if w.Code != 403 || body["error"] != "http_not_allowed" {
							t.Fatalf("unmarked plaintext bypass: status=%d", w.Code)
						}
						if w.Header().Get("Cache-Control") != "no-store" {
							t.Fatal("missing no-store")
						}
					} else {
						want := 200
						if path == "/mcp" {
							want = 204
						}
						if w.Code != want {
							t.Fatalf("explicit HTTP opt-in changed: status=%d want=%d", w.Code, want)
						}
					}
				})
			}
		}
	}
}

func TestTransportClassifierFinal2e(t *testing.T) {
	for _, tc := range []struct {
		name, peer  string
		proxies     []string
		tls, marker bool
		xfp         []string
		secure      bool
	}{
		{name: "plain", peer: "192.0.2.1:1234"},
		{name: "loopback-not-exempt", peer: "127.0.0.1:1234"},
		{name: "default-loopback-spoof", peer: "127.0.0.1:1234", xfp: []string{"https"}},
		{name: "untrusted-spoof", peer: "10.1.2.3:1234", xfp: []string{"https"}},
		{name: "direct-TLS", peer: "192.0.2.1:1234", tls: true, secure: true},
		{name: "untrusted-TLS-ignores-XFP", peer: "192.0.2.1:1234", tls: true, xfp: []string{"http"}, secure: true},
		{name: "trusted-loopback", peer: "127.0.0.1:1234", proxies: []string{"127.0.0.1/32"}, xfp: []string{"https"}, secure: true},
		{name: "trusted-LAN", peer: "10.1.2.3:1234", proxies: []string{"10.0.0.0/8"}, xfp: []string{"https"}, secure: true},
		{name: "trusted-TLS-http", peer: "10.1.2.3:1234", proxies: []string{"10.0.0.0/8"}, tls: true, xfp: []string{"http"}},
		{name: "trusted-TLS-missing", peer: "10.1.2.3:1234", proxies: []string{"10.0.0.0/8"}, tls: true},
		{name: "trusted-TLS-duplicate", peer: "10.1.2.3:1234", proxies: []string{"10.0.0.0/8"}, tls: true, xfp: []string{"https", "https"}},
		{name: "trusted-comma-list", peer: "10.1.2.3:1234", proxies: []string{"10.0.0.0/8"}, xfp: []string{"https, http"}},
		{name: "trusted-uppercase", peer: "10.1.2.3:1234", proxies: []string{"10.0.0.0/8"}, xfp: []string{"HTTPS"}},
		{name: "trusted-whitespace", peer: "10.1.2.3:1234", proxies: []string{"10.0.0.0/8"}, xfp: []string{" https"}},
		{name: "trusted-empty", peer: "10.1.2.3:1234", proxies: []string{"10.0.0.0/8"}, xfp: []string{""}},
		{name: "trusted-marker", peer: "10.1.2.3:1234", proxies: []string{"10.0.0.0/8"}, xfp: []string{"https"}, marker: true, secure: true},
		{name: "direct-TLS-marker", peer: "192.0.2.1:1234", tls: true, marker: true, secure: true},
		{name: "mapped-IP", peer: "[::ffff:127.0.0.1]:1234", proxies: []string{"127.0.0.1/32"}, xfp: []string{"https"}, secure: true},
		{name: "IPv6", peer: "[::1]:1234", proxies: []string{"::1/128"}, xfp: []string{"https"}, secure: true},
		{name: "malformed-peer", peer: "not-an-IP", proxies: []string{"0.0.0.0/0"}, xfp: []string{"https"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxies := make([]netip.Prefix, 0, len(tc.proxies))
			for _, s := range tc.proxies {
				proxies = append(proxies, netip.MustParsePrefix(s))
			}
			p := &panel{trustedProxies: proxies}
			r := httptest.NewRequest("POST", "/mcp?project=demo", nil)
			r.RemoteAddr = tc.peer
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for _, value := range tc.xfp {
				r.Header.Add("X-Forwarded-Proto", value)
			}
			r.Header.Set("Forwarded", "proto=https") // Not an accepted source of proof.
			if tc.marker {
				r.Header.Set("X-ASB-Client-Transport", "http")
			}
			if p.secure(r) != tc.secure || p.plaintext(r) != (tc.marker || !tc.secure) {
				t.Fatal("classifier disagrees with final2e")
			}
		})
	}
}

func TestTransportTrustedProxyEnforcement(t *testing.T) {
	s := directStore(t)
	raw, err := s.IssueToken("demo", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, peer           string
		trusted, tls, marker bool
		xfp                  []string
		want                 int
	}{
		{"default-none", "127.0.0.1:1234", false, false, false, []string{"https"}, 403},
		{"trusted-https", "127.0.0.1:1234", true, false, false, []string{"https"}, 204},
		{"trusted-TLS-missing", "127.0.0.1:1234", true, true, false, nil, 403},
		{"trusted-TLS-duplicate", "127.0.0.1:1234", true, true, false, []string{"https", "https"}, 403},
		{"trusted-TLS-http", "127.0.0.1:1234", true, true, false, []string{"http"}, 403},
		{"untrusted-TLS", "192.0.2.1:1234", false, true, false, nil, 204},
		{"untrusted-XFP", "192.0.2.1:1234", false, false, false, []string{"https"}, 403},
		{"marker-trusted-https", "127.0.0.1:1234", true, false, true, []string{"https"}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var proxies []netip.Prefix
			if tc.trusted {
				proxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
			}
			h := NewWithTrustedProxies(s, tokenViewBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), proxies)
			r := httptest.NewRequest("POST", "/mcp?project=demo", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("Authorization", "Bearer "+raw)
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for _, value := range tc.xfp {
				r.Header.Add("X-Forwarded-Proto", value)
			}
			if tc.marker {
				r.Header.Set("X-ASB-Client-Transport", "http")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d", w.Code, tc.want)
			}
		})
	}
}

func TestTransportIssuanceToggleRace(t *testing.T) {
	for _, path := range []string{"/oauth/token", "/oauth/session_registration"} {
		t.Run(path, func(t *testing.T) {
			s := directStore(t)
			if err := s.SetInstanceHTTPPolicy("admin", true); err != nil {
				t.Fatal(err)
			}
			parent := directParent(t, s, 8)
			form := directForm()
			if path == "/oauth/token" {
				code, err := s.BeginDeviceAuthorization("demo", parent, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", InstanceID: "fixture-client", RequestID: "device-request"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.ApproveDeviceAuthorization("admin", code.UserCode, "device-worker", false); err != nil {
					t.Fatal(err)
				}
				form = tokenForm(code.DeviceCode)
			}
			hashes := map[string]string{}
			for _, token := range s.Projects()[0].Tokens {
				hashes[token.ID] = token.TokenHash
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			s.beforeTokenExchange = func() { close(entered); <-resume }
			h := New(s, tokenViewBackend{}, http.NotFoundHandler())
			r := httptest.NewRequest("POST", path+"?project=demo", strings.NewReader(form.Encode()))
			r.RemoteAddr = "192.0.2.1:1234"
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Authorization", "Bearer "+parent)
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { w := httptest.NewRecorder(); h.ServeHTTP(w, r); result <- w }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("issuance barrier not reached")
			}
			if err := s.SetInstanceHTTPPolicy("admin", false); err != nil {
				close(resume)
				t.Fatal(err)
			}
			close(resume)
			select {
			case w := <-result:
				var body map[string]any
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if w.Code != 403 || body["error"] != "http_not_allowed" || body["access_token"] != nil {
					t.Fatal("OFF committed before issuance but token escaped")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("issuance policy check deadlocked")
			}
			after := s.Projects()[0].Tokens
			if len(after) != len(hashes) {
				t.Fatal("denial created a credential")
			}
			for _, token := range after {
				if hashes[token.ID] != token.TokenHash {
					t.Fatal("denial rotated a credential")
				}
			}
		})
	}
}

func TestTransportBrowserApprovalStillWorksOverHTTPWhenOff(t *testing.T) {
	s := directStore(t)
	raw := directParent(t, s, 8)
	code, err := s.BeginDeviceAuthorization("demo", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", InstanceID: "fixture-client", RequestID: "device-request"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	admin := loginPanelUser(t, h, "admin")
	w := admin.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {code.UserCode}, "nickname": {"browser-worker"}})
	if w.Code != 303 || s.InstanceHTTPPolicy() {
		t.Fatal("HTTP policy gated browser approval")
	}
}

func TestTransportAdminCookiesSecureBehindTrustedProxy(t *testing.T) {
	s := directStore(t)
	inner := NewWithTrustedProxies(s, tokenViewBackend{}, http.NotFoundHandler(), []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Forwarded-Proto", "https")
		inner.ServeHTTP(w, r)
	})
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "/login", nil))
	if cookies := get.Result().Cookies(); len(cookies) != 1 || !cookies[0].Secure {
		t.Fatal("form cookie not Secure")
	}
	admin := loginPanelUser(t, h, "admin")
	if !admin.cookie.Secure {
		t.Fatal("admin cookie not Secure")
	}
	out := admin.call("POST", "/logout", nil)
	if cookies := out.Result().Cookies(); out.Code != 303 || len(cookies) != 1 || !cookies[0].Secure || cookies[0].MaxAge != -1 {
		t.Fatal("logout cookie not Secure")
	}
}

func TestTransportDenialLogIsBoundedAndExcludesRawHeaders(t *testing.T) {
	var output bytes.Buffer
	prior := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(prior)
	p := &panel{}
	for range 2 {
		r := httptest.NewRequest("POST", "/mcp?project=demo&secret=NEVER_LOG_QUERY", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("X-Forwarded-Proto", "NEVER_LOG_HEADER")
		r.Header.Set("Authorization", "NEVER_LOG_AUTH")
		p.denyTransport(httptest.NewRecorder(), r, "demo")
	}
	text := output.String()
	if strings.Count(text, "http-transport-denied") != 1 || !strings.Contains(text, "--trusted-proxy") || !strings.Contains(text, "untrusted-forwarded") || strings.Contains(text, "NEVER_LOG") {
		t.Fatal("denial log unbounded or exposed raw data")
	}
}
