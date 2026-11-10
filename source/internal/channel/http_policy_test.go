package channel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type policyRoundTripper func(*http.Request) (*http.Response, error)

func (f policyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPProjectTokenPolicyPrecedesBearer(t *testing.T) {
	for name, policy := range map[string]func(http.ResponseWriter, *http.Request){
		"false": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"project": "brk", "allow_http": false})
		},
		"not-found": func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
		"malformed": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not-json")) },
		"wrong-project": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"project": "other", "allow_http": true})
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
	} {
		t.Run(name, func(t *testing.T) {
			var policyCalls, bearerCalls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/transport-policy" {
					policyCalls.Add(1)
					if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
						t.Errorf("policy request carried credentials: %v", r.Header)
					}
					policy(w, r)
					return
				}
				if r.Header.Get("Authorization") != "" {
					bearerCalls.Add(1)
				}
				writeJSON(w, http.StatusOK, map[string]any{"device_code": "code", "user_code": "USER", "verification_uri": "/auth/code", "expires_in": 60})
			}))
			defer s.Close()
			c := newDeviceAuthClient(s.URL+"/mcp?project=brk", "asb_p_token", s.Client())
			if _, err := c.requestCode(context.Background(), deviceCodeRequest{}); err == nil {
				t.Fatal("HTTP project token request was allowed")
			}
			if policyCalls.Load() != 1 || bearerCalls.Load() != 0 {
				t.Fatalf("policy=%d bearer=%d, want 1 and 0", policyCalls.Load(), bearerCalls.Load())
			}
		})
	}
}

func TestHTTPProjectTokenPolicyAllowsAndMarksBearer(t *testing.T) {
	var policyCalls, bearerCalls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/transport-policy":
			policyCalls.Add(1)
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Errorf("policy request carried credentials: %v", r.Header)
			}
			writeJSON(w, http.StatusOK, map[string]any{"project": "brk", "allow_http": true})
		case "/oauth/device_authorization":
			bearerCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer asb_p_token" || r.Header.Get("X-ASB-Client-Transport") != "http" {
				t.Errorf("credential request headers: %v", r.Header)
			}
			writeJSON(w, http.StatusOK, map[string]any{"device_code": "code", "user_code": "USER", "verification_uri": "/auth/code", "expires_in": 60})
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c := newDeviceAuthClient(s.URL+"/mcp?project=brk", "asb_p_token", s.Client())
	if _, err := c.requestCode(context.Background(), deviceCodeRequest{}); err != nil {
		t.Fatal(err)
	}
	if policyCalls.Load() != 1 || bearerCalls.Load() != 1 {
		t.Fatalf("policy=%d bearer=%d, want 1 and 1", policyCalls.Load(), bearerCalls.Load())
	}
}

func TestParseClientOptionsAcceptsHTTPProjectToken(t *testing.T) {
	if _, err := parseClientOptions([]string{"--broker-url", "http://broker/mcp?project=brk", "--token", "asb_p_token"}); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerUpstreamHTTPPolicyGuardsSessionBearer(t *testing.T) {
	for name, allow := range map[string]bool{"denied": false, "allowed": true} {
		t.Run(name, func(t *testing.T) {
			var policies, bearerCalls atomic.Int32
			roundTrip := policyRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/oauth/transport-policy" {
					policies.Add(1)
					if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
						t.Errorf("policy request carried credentials: %v", r.Header)
					}
					body := `{"project":"brk","allow_http":false}`
					if allow {
						body = `{"project":"brk","allow_http":true}`
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				}
				bearerCalls.Add(1)
				if r.Header.Get("Authorization") != "Bearer asb_s_session" || r.Header.Get("X-ASB-Client-Transport") != "http" {
					t.Errorf("credential request headers: %v", r.Header)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
			})
			u := newTestBrokerUpstream(context.Background(), "http://broker/mcp?project=brk")
			u.transport = roundTrip
			u.setBearerToken("asb_s_session")
			request, _ := http.NewRequest(http.MethodPost, "http://broker/mcp?project=brk", nil)
			_, err := (headerTransport{header: u.header, next: roundTrip, policy: u.policy}).RoundTrip(request)
			if !allow && err == nil {
				t.Fatal("denied policy sent the session bearer")
			}
			if allow && err != nil {
				t.Fatal(err)
			}
			if policies.Load() != 1 || bearerCalls.Load() != map[bool]int32{false: 0, true: 1}[allow] {
				t.Fatalf("policy=%d bearer=%d", policies.Load(), bearerCalls.Load())
			}
		})
	}

	// Legacy unprefixed credentials preserve their existing HTTP behavior.
	u := newTestBrokerUpstream(context.Background(), "http://broker/mcp?project=brk")
	u.setBearerToken("legacy-session")
	if u.policy != nil {
		t.Fatal("legacy session credential unexpectedly needs HTTP policy")
	}
	if parsed, err := url.Parse(u.endpoint); err != nil || parsed.Scheme != "http" {
		t.Fatal(parsed, err)
	}
}
