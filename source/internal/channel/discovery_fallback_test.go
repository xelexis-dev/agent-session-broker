package channel

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDirectRedirectStartsManualDeviceFlow(t *testing.T) {
	auth := newDeviceAuthServer(t)
	original := auth.server.Config.Handler
	var direct, escaped atomic.Int32
	auth.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/capabilities":
			if r.Header.Get("Authorization") != "" {
				t.Error("discovery carried bearer")
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		case "/login":
			escaped.Add(1)
			w.WriteHeader(200)
		case "/oauth/session_registration":
			direct.Add(1)
			w.WriteHeader(403)
		default:
			original.ServeHTTP(w, r)
		}
	})
	f := newDeviceModeFixture(t, auth, newSwitchableUpstream(t), t.TempDir(), singleIdentity())
	waitFor(t, "manual device request or prior discovery failure", func() bool { return f.codeRequests() > 0 || f.status("")["status"] == authFailed })
	if f.codeRequests() != 1 {
		t.Fatal("303 discovery ended in auth failed with zero manual device requests")
	}
	if f.status("")["status"] != authPending || direct.Load() != 0 || escaped.Load() != 0 {
		t.Fatal("manual fallback changed or followed redirect")
	}
}

func TestDirectDefiniteFailureStartsManual(t *testing.T) {
	for _, status := range []int{401, 403, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			original := auth.server.Config.Handler
			auth.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/capabilities" {
					if r.Header.Get("Authorization") != "" {
						t.Error("discovery carried bearer")
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte("<html>login</html>"))
					return
				}
				original.ServeHTTP(w, r)
			})
			f := newDeviceModeFixture(t, auth, newSwitchableUpstream(t), t.TempDir(), singleIdentity())
			waitFor(t, "manual fallback", func() bool { return f.codeRequests() == 1 })
			if f.status("")["status"] != authPending {
				t.Fatal("definite discovery answer failed authorization")
			}
		})
	}
}

func TestDirectDiscoveryTransientBackoffAndFallback(t *testing.T) {
	for _, kind := range []string{"5xx-then-success", "network-then-success", "persistent-5xx", "persistent-network"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("discovery carried bearer")
				}
				n := calls.Add(1)
				if n == 1 || strings.HasPrefix(kind, "persistent") {
					if strings.Contains(kind, "network") {
						panic(http.ErrAbortHandler)
					}
					w.WriteHeader(503)
					return
				}
				writeJSON(w, 200, map[string]any{"version": 1, "capabilities": []string{"direct-registration-v1", "scoped-session-v1"}})
			}))
			defer server.Close()
			client := newDeviceAuthClient(server.URL+"/mcp?project=brk", "synthetic-project", server.Client())
			clock := installRecordingAuthClock(client)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			supported, err := client.discover(ctx)
			persistent := strings.HasPrefix(kind, "persistent")
			wantCalls := int32(2)
			if persistent {
				wantCalls = 3
			}
			wantDelays := []time.Duration{time.Second}
			if persistent {
				wantDelays = append(wantDelays, 2*time.Second)
			}
			clock.assertDelays(t, wantDelays...)
			if err != nil || supported == persistent || calls.Load() != wantCalls {
				t.Fatal("transient discovery did not retry boundedly then recover or fall back")
			}
		})
	}
}

func TestRestrictedDiscoveryWithoutPositiveScopedEvidenceFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"redirect", 303, ""}, {"unauthorized", 401, "<html>login</html>"}, {"forbidden", 403, ""}, {"html", 200, "<html>login</html>"}, {"version", 200, `{"version":2,"capabilities":["scoped-session-v1"]}`}, {"no-scoped", 200, `{"version":1,"capabilities":["direct-registration-v1"]}`}, {"oversized", 200, strings.Repeat("x", (64<<10)+1)}, {"transient", 503, ""}, {"network", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("scope discovery carried bearer")
				}
				if tc.status == 0 {
					panic(http.ErrAbortHandler)
				}
				w.Header().Set("Location", "/login")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			auth := &sessionAuth{state: clientstate.SessionState{Credential: &clientstate.SessionCredential{AccessToken: "synthetic-child", Scopes: []string{"session:register"}}}}
			client := newDeviceAuthClient(server.URL+"/mcp?project=brk", "synthetic-parent", server.Client())
			clock := installRecordingAuthClock(client)
			opts := durableOptions{device: &deviceAuthOptions{authorizer: client}}
			if err := opts.verifyStoredScopes(t.Context(), auth); !errors.Is(err, errScopeUpgrade) {
				t.Fatal("restricted state silently fell back without positive scoped evidence")
			}
			if tc.status == 503 || tc.status == 0 {
				clock.assertDelays(t, time.Second, 2*time.Second)
			} else {
				clock.assertDelays(t)
			}
		})
	}
}

func TestDirectDiscoveryCancellationStopsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); cancel(); w.WriteHeader(503) }))
	defer server.Close()
	client := newInstantDeviceAuthClient(server.URL+"/mcp?project=brk", "synthetic-parent", server.Client())
	if _, err := client.discover(ctx); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatal("canceled discovery continued retry/fallback")
	}
}

func TestDirectUnavailableDiscoveryStillStartsManual(t *testing.T) {
	for _, network := range []bool{false, true} {
		name := "server-error"
		if network {
			name = "network-error"
		}
		t.Run(name, func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			original := auth.server.Config.Handler
			var discovery atomic.Int32
			auth.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/capabilities" {
					discovery.Add(1)
					if network {
						panic(http.ErrAbortHandler)
					}
					w.WriteHeader(503)
					return
				}
				original.ServeHTTP(w, r)
			})
			f := newDeviceModeFixture(t, auth, newSwitchableUpstream(t), t.TempDir(), singleIdentity())
			waitForWithin(t, 8*time.Second, "manual after bounded discovery failure", func() bool { return f.codeRequests() == 1 })
			f.authClock.assertDelays(t, time.Second, 2*time.Second)
			if discovery.Load() != 3 || f.status("")["status"] != authPending {
				t.Fatal("discovery failure stopped manual authorization or retried without bound")
			}
		})
	}
}
