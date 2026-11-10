package channel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// leakRecorder stands for a server a redirect could send a bearer to. Any
// request that reaches it with an Authorization header is a token leak.
type leakRecorder struct {
	mu   sync.Mutex
	hits []string
}

func (l *leakRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.hits = append(l.hits, r.URL.Path+"|"+r.Header.Get("Authorization"))
		l.mu.Unlock()
		// Look like a successful device authorization so a followed redirect
		// would be mistaken for success.
		writeJSON(w, 200, map[string]any{"device_code": "leaked", "user_code": "LEAK-CODE", "verification_uri": "/auth/code", "expires_in": 600, "interval": 5, "access_token": "asb_s_leaked", "token_type": "Bearer", "nickname": "leaked"})
	})
}

func (l *leakRecorder) leaked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.hits...)
}

// redirectingOrigin answers every request with a redirect to target and
// records that the original request carried the expected bearer.
func redirectingOrigin(t *testing.T, tls bool, code int, target string, sawBearer *sync.Map) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer.Store(r.URL.Path, r.Header.Get("Authorization"))
		http.Redirect(w, r, target, code)
	})
	var server *httptest.Server
	if tls {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	t.Cleanup(server.Close)
	return server
}

func TestDeviceAuthClientNeverFollowsRedirects(t *testing.T) {
	leak := &leakRecorder{}
	leakServer := httptest.NewServer(leak.handler())
	defer leakServer.Close()
	other := httptest.NewTLSServer(leak.handler())
	defer other.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		for name, target := range map[string]func(origin *httptest.Server) string{
			"https-to-http-other-port": func(*httptest.Server) string { return leakServer.URL + "/mcp" },
			"cross-origin-https":       func(*httptest.Server) string { return other.URL + "/mcp" },
			"same-origin-mcp":          func(origin *httptest.Server) string { return origin.URL + "/mcp?project=brk" },
		} {
			t.Run(fmt.Sprintf("%d/%s/injected", code, name), func(t *testing.T) {
				var saw sync.Map
				origin := redirectingOrigin(t, true, code, "", &saw)
				origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					saw.Store(r.URL.Path, r.Header.Get("Authorization"))
					if r.URL.Path == "/mcp" {
						leak.handler().ServeHTTP(w, r)
						return
					}
					http.Redirect(w, r, target(origin), code)
				})
				before := len(leak.leaked())
				client := newDeviceAuthClient(origin.URL+"/mcp?project=brk", "asb_p_secret", origin.Client())
				if _, err := client.requestCode(context.Background(), deviceCodeRequest{Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", ClientID: "pc-one", RequestID: "req-1"}); err == nil {
					t.Fatal("redirected code request reported success")
				}
				if grant, outcome, err := client.redeem(context.Background(), "dev-secret"); outcome == tokenGranted || err == nil || grant.AccessToken != "" {
					t.Fatalf("redirected token request reported success: %v %v", outcome, err)
				}
				if bearer, _ := saw.Load("/oauth/device_authorization"); bearer != "Bearer asb_p_secret" {
					t.Fatalf("origin did not receive the original request: %v", bearer)
				}
				if leaked := leak.leaked(); len(leaked) != before {
					t.Fatalf("bearer followed the redirect: %v", leaked[before:])
				}
				if _, followed := saw.Load("/mcp"); followed {
					t.Fatal("same-origin /mcp redirect was followed")
				}
			})
		}
	}
	// The default client (no injected http.Client) applies the same policy.
	t.Run("default-client", func(t *testing.T) {
		var saw sync.Map
		origin := redirectingOrigin(t, false, 307, leakServer.URL+"/mcp", &saw)
		before := len(leak.leaked())
		client := newDeviceAuthClient(origin.URL+"/mcp?project=brk", "asb_p_secret", nil)
		if _, err := client.requestCode(context.Background(), deviceCodeRequest{Harness: "codex", SessionID: "22222222-2222-4222-8222-222222222222", ClientID: "pc-one", RequestID: "req-2"}); err == nil {
			t.Fatal("redirected code request reported success")
		}
		if leaked := leak.leaked(); len(leaked) != before {
			t.Fatalf("default client followed the redirect: %v", leaked[before:])
		}
	})
}

func TestBrokerUpstreamNeverFollowsRedirects(t *testing.T) {
	leak := &leakRecorder{}
	leakServer := httptest.NewServer(leak.handler())
	defer leakServer.Close()
	for _, tc := range []struct {
		name string
		tls  bool
	}{{"tls-origin-injected-transport", true}, {"http-origin-default-transport", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var saw sync.Map
			origin := redirectingOrigin(t, tc.tls, 307, leakServer.URL+"/mcp?project=brk", &saw)
			origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/transport-policy" {
					writeJSON(w, http.StatusOK, map[string]any{"project": "brk", "allow_http": true})
					return
				}
				saw.Store(r.URL.Path, r.Header.Get("Authorization"))
				http.Redirect(w, r, leakServer.URL+"/mcp?project=brk", http.StatusTemporaryRedirect)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			u := newTestBrokerUpstream(ctx, origin.URL+"/mcp?project=brk")
			defer u.Close()
			if tc.tls {
				u.transport = origin.Client().Transport
			}
			u.setBearerToken("asb_s_secret")
			before := len(leak.leaked())
			if _, err := u.session(ctx); err == nil {
				t.Fatal("redirected MCP handshake reported success")
			}
			if bearer, _ := saw.Load("/mcp"); bearer != "Bearer asb_s_secret" {
				t.Fatalf("origin did not receive the original request: %v", bearer)
			}
			for _, hit := range leak.leaked()[before:] {
				if strings.Contains(hit, "Bearer") {
					t.Fatalf("session credential followed the redirect: %s", hit)
				}
			}
			if leaked := leak.leaked(); len(leaked) != before {
				t.Fatalf("MCP upstream followed the redirect: %v", leaked[before:])
			}
		})
	}
}
