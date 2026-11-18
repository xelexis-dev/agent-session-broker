package controlpanel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestConnectParentDeviceExchange(t *testing.T) {
	s := connectFixture(t)
	raw, _ := connectIssue(t, s, "manual", 0)
	h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	f := &deviceHTTPFixture{t: t, store: s, h: h}
	w, body := f.rest("POST", deviceAuthorizationPath+"?project=demo", raw, deviceForm("connect-http-session", "connect-http-request"))
	if w.Code != 200 {
		t.Fatalf("HTTP layer rejected connect parent: status=%d", w.Code)
	}
	code, _ := body["user_code"].(string)
	device, _ := body["device_code"].(string)
	owner := loginPanelUser(t, h, "admin")
	approved := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {code}, "nickname": {"http-child"}})
	if approved.Code != 303 {
		t.Fatalf("panel approval status=%d", approved.Code)
	}
	w, body = f.rest("POST", deviceTokenPath+"?project=demo", raw, tokenForm(device))
	if w.Code != 200 {
		t.Fatalf("HTTP exchange status=%d", w.Code)
	}
	child, _ := body["access_token"].(string)
	w, _ = f.rest("GET", "/mcp?project=demo", child, nil)
	if w.Code != 204 {
		t.Fatalf("HTTP child status=%d", w.Code)
	}
}

func connectHTTPApproved(t *testing.T, s *Store, project, raw, name string) (DeviceTokenResponse, http.Handler) {
	t.Helper()
	h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	f := &deviceHTTPFixture{t: t, store: s, h: h}
	w, body := f.rest("POST", deviceAuthorizationPath+"?project="+project, raw, deviceForm(name, "request-"+name))
	if w.Code != 200 {
		t.Fatalf("HTTP fixture device status=%d", w.Code)
	}
	code, _ := body["user_code"].(string)
	device, _ := body["device_code"].(string)
	owner := loginPanelUser(t, h, "admin")
	approved := owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {code}, "nickname": {name}})
	if approved.Code != 303 {
		t.Fatalf("HTTP fixture approval status=%d", approved.Code)
	}
	w, _ = f.rest("POST", deviceTokenPath+"?project="+project, raw, tokenForm(device))
	if w.Code != 200 {
		t.Fatalf("HTTP fixture exchange status=%d", w.Code)
	}
	var token DeviceTokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	return token, h
}

func TestConnectProtocolMetadataAndLegacyResponses(t *testing.T) {
	s := connectFixture(t)
	raw, parent := connectIssue(t, s, "manual", 0)
	token, _ := connectHTTPApproved(t, s, "demo", raw, "manual-http")
	if token.ParentKind != "user-connect" || !token.ParentExpiresAt.Equal(parent.ExpiresAt) || token.Project != "demo" || token.ProjectGeneration != s.ProjectGeneration("demo") {
		t.Fatal("connect device parent metadata missing")
	}
	grant, _, ok := s.authorize("demo", token.AccessToken)
	if !ok || grant.ParentKind != "user-connect" || !grant.ParentExpiresAt.Equal(parent.ExpiresAt) {
		t.Fatal("grant parent metadata missing")
	}
	auto, autoParent := connectIssue(t, s, "auto", 0)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	f := &deviceHTTPFixture{t: t, store: s, h: h}
	w, _ := f.rest("POST", directPath+"?project=demo", auto, directForm())
	if w.Code != 200 {
		t.Fatalf("connect direct status=%d", w.Code)
	}
	var direct DirectResponse
	if json.Unmarshal(w.Body.Bytes(), &direct) != nil || direct.ParentKind != "user-connect" || !direct.ParentExpiresAt.Equal(autoParent.ExpiresAt) || direct.Project != "demo" || direct.ProjectGeneration != s.ProjectGeneration("demo") {
		t.Fatal("connect direct metadata missing")
	}
	// Exact historical JSON (including field order); optional connect fields never appear on p responses.
	old := DeviceTokenResponse{AccessToken: "synthetic", TokenType: "Bearer", ExpiresIn: 123, GrantID: "grant", Nickname: "agent"}
	encoded, _ := json.Marshal(old)
	if string(encoded) != `{"access_token":"synthetic","token_type":"Bearer","expires_in":123,"session_grant_id":"grant","nickname":"agent"}` {
		t.Fatal("project-token response bytes changed")
	}
	pRaw, err := s.IssueProjectTokenFor("admin", "demo", "legacy-client-1.3.0")
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := connectHTTPApproved(t, s, "demo", pRaw, "legacy-http")
	if legacy.ParentKind != "" || !legacy.ParentExpiresAt.IsZero() || legacy.Project != "" || legacy.ProjectGeneration != "" || !s.Authorize("demo", legacy.AccessToken) {
		t.Fatal("legacy project HTTP flow changed")
	}
	legacyGrant, _, ok := s.authorize("demo", legacy.AccessToken)
	if !ok || legacyGrant.ParentKind != "" || !legacyGrant.ParentExpiresAt.IsZero() {
		t.Fatal("legacy grant metadata changed")
	}
}

func TestConnectProtocolCapabilityAndSwitch(t *testing.T) {
	s := connectFixture(t)
	raw, _ := connectIssue(t, s, "manual", 0)
	f := &deviceHTTPFixture{t: t, store: s, h: New(s, &panelBackend{}, http.NotFoundHandler())}
	for _, enabled := range []bool{false, true} {
		for _, allow := range []bool{false, true} {
			if err := s.setConnectPolicy("admin", enabled); err != nil {
				t.Fatal(err)
			}
			if err := s.SetInstanceHTTPPolicy("admin", allow); err != nil {
				t.Fatal(err)
			}
			w, body := f.rest("GET", capabilityPath, "", nil)
			if w.Code != 200 || body["version"] != float64(1) || body["connect_http_allowed"] != (enabled && allow) {
				t.Fatal("capability switch/policy mismatch")
			}
			has := false
			for _, v := range body["capabilities"].([]any) {
				if v == "connect-parent-v1" {
					has = true
				}
			}
			if has != enabled {
				t.Fatal("connect capability readiness mismatch")
			}
			if !enabled {
				for _, path := range []string{deviceTokenPath, directPath} {
					form := tokenForm("synthetic")
					if path == directPath {
						form = directForm()
					}
					w, body := f.rest("POST", path+"?project=demo", raw, form)
					if w.Code != 403 || body["error"] != "connect_disabled" {
						t.Fatalf("OFF %s status=%d", path, w.Code)
					}
				}
			}
			w, body = f.rest("POST", deviceAuthorizationPath+"?project=demo", raw, deviceForm("switch", "switch"))
			if enabled && w.Code != 200 || !enabled && (w.Code != 403 || body["error"] != "connect_disabled") {
				t.Fatalf("switch device status=%d", w.Code)
			}
		}
	}
	management, _, err := s.IssueOwnUserToken("admin", s.data.Users["admin"].Revision, "management", 3600, []string{"read"}, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	w, body := f.rest("POST", deviceAuthorizationPath+"?project=demo", management, deviceForm("management", "management"))
	if w.Code != 401 || body["error"] != "connect_forbidden" {
		t.Fatalf("management user token accepted: %d", w.Code)
	}
	for _, path := range []string{deviceTokenPath, directPath} {
		form := tokenForm("synthetic")
		if path == directPath {
			form = directForm()
		}
		w, body := f.rest("POST", path+"?project=demo", management, form)
		if w.Code != 401 || body["error"] != "connect_forbidden" {
			t.Fatalf("management %s status=%d", path, w.Code)
		}
	}
}

func TestConnectProtocolTransportAndCommitPolicy(t *testing.T) {
	cases := []struct {
		name, scheme, xfp, marker string
		trusted                   bool
		want                      int
	}{
		{name: "tls", scheme: "https", want: 200},
		{name: "plain", scheme: "http", want: 403},
		{name: "untrusted-xfp", scheme: "http", xfp: "https", want: 403},
		{name: "trusted-xfp", scheme: "http", xfp: "https", trusted: true, want: 200},
		{name: "trusted-missing-xfp", scheme: "https", trusted: true, want: 403},
		{name: "client-marked", scheme: "https", marker: "http", want: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := connectFixture(t)
			raw, _ := connectIssue(t, s, "manual", 0)
			var proxies []netip.Prefix
			if tc.trusted {
				proxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}
			}
			h := NewWithTrustedProxies(s, &panelBackend{}, http.NotFoundHandler(), proxies)
			makeRequest := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", tc.scheme+"://broker"+deviceAuthorizationPath+"?project=demo", strings.NewReader(deviceForm("transport", "transport").Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Authorization", "Bearer "+raw)
				if tc.xfp != "" {
					r.Header.Set("X-Forwarded-Proto", tc.xfp)
				}
				if tc.marker != "" {
					r.Header.Set("X-ASB-Client-Transport", tc.marker)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			w := makeRequest()
			if w.Code != tc.want {
				t.Fatalf("transport status=%d want=%d", w.Code, tc.want)
			}
			if tc.want == 403 && !strings.Contains(w.Body.String(), "connect_http_denied") {
				t.Fatal("wrong transport error")
			}
			if err := s.SetInstanceHTTPPolicy("admin", true); err != nil {
				t.Fatal(err)
			}
			if w = makeRequest(); w.Code != 200 {
				t.Fatalf("HTTP opt-in status=%d", w.Code)
			}
		})
	}
	for _, path := range []string{deviceAuthorizationPath, deviceTokenPath, directPath} {
		t.Run("commit"+path, func(t *testing.T) {
			s := connectFixture(t)
			mode := "manual"
			if path == directPath {
				mode = "auto"
			}
			raw, _ := connectIssue(t, s, mode, 0)
			h := New(s, &panelBackend{}, http.NotFoundHandler())
			form := deviceForm("commit", "commit")
			if path == deviceTokenPath {
				d := connectPending(t, s, "demo", raw, "policy")
				if _, err := s.approvePanelDeviceAuthorization("admin", d.UserCode, "policy", false); err != nil {
					t.Fatal(err)
				}
				form = tokenForm(d.DeviceCode)
			} else if path == directPath {
				form = directForm()
			}
			if err := s.SetInstanceHTTPPolicy("admin", true); err != nil {
				t.Fatal(err)
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			s.beforeTokenExchange = func() { close(entered); <-resume }
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequest("POST", "http://broker"+path+"?project=demo", strings.NewReader(form.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Authorization", "Bearer "+raw)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				result <- w
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("commit barrier not reached")
			}
			if err := s.SetInstanceHTTPPolicy("admin", false); err != nil {
				t.Fatal(err)
			}
			close(resume)
			w := <-result
			if w.Code != 403 || !strings.Contains(w.Body.String(), "connect_http_denied") {
				t.Fatalf("policy race status=%d", w.Code)
			}
		})
	}
}

func TestConnectProtocolWrongEndpointRevocation(t *testing.T) {
	for _, tc := range []struct {
		name, path, scheme  string
		disabled, crossSite bool
	}{
		{name: "TLS", path: "/mcp?project=demo", scheme: "https"},
		{name: "no-project", path: "/mcp", scheme: "https"},
		{name: "invalid-query", path: "/mcp?%zz", scheme: "https"},
		{name: "missing-project", path: "/mcp?project=absent", scheme: "https"},
		{name: "plain-OFF", path: "/mcp?project=demo", scheme: "http", disabled: true},
		{name: "cross-site", path: "/mcp?project=demo", scheme: "https", crossSite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := connectFixture(t)
			raw, parent := connectIssue(t, s, "manual", 0)
			token, h := connectHTTPApproved(t, s, "demo", raw, "wrong-endpoint")
			grant, _, ok := s.authorize("demo", token.AccessToken)
			if !ok {
				t.Fatal("fixture grant")
			}
			if tc.disabled {
				if err := s.setConnectPolicy("admin", false); err != nil {
					t.Fatal(err)
				}
			}
			r := httptest.NewRequest("POST", tc.scheme+"://broker"+tc.path, nil)
			r.Header.Set("Authorization", "Bearer "+raw)
			if tc.crossSite {
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatalf("F3 status=%d", w.Code)
			}
			rows, _ := s.listRecords()
			if _, ok := rows[parent.key()]; ok {
				t.Fatal("F3 retained parent row")
			}
			select {
			case <-grant.Revoked:
			default:
				t.Fatal("F3 retained child stream")
			}
			found := 0
			for _, e := range s.connect.audits {
				if e.ParentID == parent.ID && e.Kind == "token-revoked" {
					if e.Reason != "wrong-endpoint" {
						t.Fatal("F3 audit misclassified")
					}
					found++
				}
			}
			if found != 1 {
				t.Fatal("F3 audit missing or duplicate")
			}
		})
	}
	t.Run("management-not-revoked", func(t *testing.T) {
		s := connectFixture(t)
		raw, view, err := s.IssueOwnUserToken("admin", s.data.Users["admin"].Revision, "management", 3600, []string{"read"}, "fixture", false)
		if err != nil {
			t.Fatal(err)
		}
		f := &deviceHTTPFixture{t: t, store: s, h: New(s, &panelBackend{}, http.NotFoundHandler())}
		w, _ := f.rest("GET", "/mcp?project=demo", raw, nil)
		if w.Code != 401 {
			t.Fatal("management MCP status")
		}
		rows, _ := s.listRecords()
		if _, ok := rows["user-tokens/admin/"+view.ID]; !ok {
			t.Fatal("management token revoked")
		}
	})
	t.Run("batch-failure", func(t *testing.T) {
		s := connectFixture(t)
		raw, parent := connectIssue(t, s, "manual", 0)
		token, h := connectHTTPApproved(t, s, "demo", raw, "atomic-f3")
		grant, _, _ := s.authorize("demo", token.AccessToken)
		write := s.writeBatch
		s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
		f := &deviceHTTPFixture{t: t, store: s, h: h}
		w, body := f.rest("GET", "/mcp", raw, nil)
		if w.Code != 503 || body["error"] != "server_error" {
			t.Fatal("F3 failed storage did not return fixed error")
		}
		select {
		case <-grant.Revoked:
			t.Fatal("F3 notified before commit")
		default:
		}
		rows, _ := s.listRecords()
		if _, ok := rows[parent.key()]; !ok {
			t.Fatal("failed F3 removed row")
		}
		s.writeBatch = write
		w, _ = f.rest("GET", "/mcp", raw, nil)
		if w.Code != 401 {
			t.Fatal("F3 retry")
		}
	})
}

func TestConnectProtocolPlainExchange(t *testing.T) {
	for _, path := range []string{deviceTokenPath, directPath} {
		t.Run(path, func(t *testing.T) {
			s := connectFixture(t)
			mode := "manual"
			if path == directPath {
				mode = "auto"
			}
			raw, _ := connectIssue(t, s, mode, 0)
			form := directForm()
			if path == deviceTokenPath {
				d := connectPending(t, s, "demo", raw, "plain-exchange")
				if _, err := s.approvePanelDeviceAuthorization("admin", d.UserCode, "plain-exchange", false); err != nil {
					t.Fatal(err)
				}
				form = tokenForm(d.DeviceCode)
			}
			h := New(s, &panelBackend{}, http.NotFoundHandler())
			call := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "http://broker"+path+"?project=demo", strings.NewReader(form.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Authorization", "Bearer "+raw)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			if w := call(); w.Code != 403 || !strings.Contains(w.Body.String(), "connect_http_denied") {
				t.Fatal("plain exchange accepted with AllowHTTP OFF")
			}
			if err := s.SetInstanceHTTPPolicy("admin", true); err != nil {
				t.Fatal(err)
			}
			w := call()
			if w.Code != 200 {
				t.Fatalf("plain exchange with AllowHTTP ON status=%d", w.Code)
			}
			var result DeviceTokenResponse
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || !s.Authorize("demo", result.AccessToken) {
				t.Fatal("plain exchange child rejected")
			}
		})
	}
}

func TestConnectProtocolAPIBearerApprovalRefused(t *testing.T) {
	s := connectFixture(t)
	raw, _ := connectIssue(t, s, "manual", 0)
	d := connectPending(t, s, "demo", raw, "api-denied")
	if _, err := s.ApproveDeviceAuthorization("admin", d.UserCode, "api-denied", false); !errors.Is(err, ErrForbidden) {
		t.Fatal("shared store approved user parent", err)
	}
	if err := s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	apiRaw, _, err := s.IssueOwnUserToken("admin", s.data.Users["admin"].Revision, "approve", 3600, []string{"approve"}, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	body, _ := json.Marshal(map[string]string{"user_code": d.UserCode, "action": "approve", "nickname": "api-denied"})
	r := httptest.NewRequest("POST", "https://broker/api/v1/device-approvals/decision", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+apiRaw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("API bearer approval status=%d", w.Code)
	}
	if _, err = s.approvePanelDeviceAuthorization("admin", d.UserCode, "panel-approved", false); err != nil {
		t.Fatal("panel approval refused", err)
	}
}

func TestConnectProtocolWrongEndpointLargeParentAudit(t *testing.T) {
	s := connectFixture(t)
	raw, parent := connectIssue(t, s, "manual", 300)
	s.mu.Lock()
	candidate := s.data
	candidate.Projects = maps.Clone(s.data.Projects)
	now := s.now().UTC()
	for i := 0; i < 257; i++ {
		name := "demo"
		if i == 256 {
			name = "other"
		}
		p := candidate.Projects[name]
		p.Tokens = append(slices.Clone(p.Tokens), SessionToken{Kind: tokenSession, ScopeVersion: access.ScopeVersion, Scopes: access.LegacyV1Scopes(), ParentID: parent.ID, ID: rand.Text() + rand.Text(), OwnerID: "admin", Nickname: fmt.Sprintf("child-%03d", i), IssuedAt: now, ExpiresAt: now.Add(sessionLifetime), BoundIdentity: fmt.Sprintf(`["codex","child-%03d","pc"]`, i), TokenHash: digest(rand.Text())})
		candidate.Projects[name] = p
	}
	err := s.commit(candidate)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	f := &deviceHTTPFixture{t: t, store: s, h: New(s, &panelBackend{}, http.NotFoundHandler())}
	w, _ := f.rest("GET", "/mcp", raw, nil)
	if w.Code != 401 {
		t.Fatal("large parent F3 status")
	}
	found := false
	rows, _ := s.listRecords()
	for key, raw := range rows {
		if !strings.HasPrefix(key, "connect-audit/") {
			continue
		}
		var e connectAudit
		if json.Unmarshal(raw, &e) == nil && e.ParentID == parent.ID && e.Kind == "token-revoked" && e.Reason == "wrong-endpoint" {
			found = true
		}
	}
	if !found {
		t.Fatal("child audit flood evicted newest F3 parent revoke")
	}
}

type connectRateBackend struct {
	*panelBackend
	live         bool
	holderChecks int
}

func (b *connectRateBackend) InspectDirectRegistration(_ context.Context, _ string, _ DirectRequest) (DirectProof, error) {
	if !b.live {
		return DirectProof{Absent: true}, nil
	}
	return DirectProof{RegistrationID: "existing-registration", OwnerID: "admin", Name: "worker", Revision: 1, AdmissionGrantID: "previous-grant"}, nil
}
func (b *connectRateBackend) CheckRegistrationHolder(_ context.Context, _ string, _ DirectRequest) error {
	b.holderChecks++
	return nil
}

func TestConnectProtocolDirectRateRetry(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint("live-holder=", live), func(t *testing.T) {
			s := connectFixture(t)
			now := s.now()
			s.now = func() time.Time { return now }
			raw, _ := connectIssue(t, s, "auto", 0)
			backend := &connectRateBackend{panelBackend: &panelBackend{}, live: live}
			h := New(s, backend, http.NotFoundHandler())
			f := &deviceHTTPFixture{t: t, store: s, h: h}
			form := directForm()
			if live {
				form.Set("registration_id", "existing-registration")
				form.Set("resume_secret", "synthetic-proof")
			}
			successes := 0
			for ; successes < 30; successes++ {
				w, _ := f.rest("POST", directPath+"?project=demo", raw, form)
				if w.Code == 200 {
					continue
				}
				if w.Code != 429 {
					t.Fatalf("exhaustion status=%d", w.Code)
				}
				seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
				if err != nil || seconds <= 0 {
					t.Fatal("missing retry delay")
				}
				now = now.Add(time.Duration(seconds) * time.Second)
				retry, _ := f.rest("POST", directPath+"?project=demo", raw, form)
				if retry.Code != 200 {
					t.Fatalf("honouring Retry-After still denied: successes=%d status=%d", successes, retry.Code)
				}
				break
			}
			if successes != 20 {
				t.Fatalf("one debit per direct request: successful=%d want=20", successes)
			}
			for i := 0; i < 3; i++ {
				now = now.Add(3 * time.Second)
				w, _ := f.rest("POST", directPath+"?project=demo", raw, form)
				if w.Code != 200 {
					t.Fatalf("refilled retry starved: status=%d", w.Code)
				}
			}
			if live && backend.holderChecks == 0 {
				t.Fatal("live holder path not exercised")
			}
		})
	}
}

func TestConnectProtocolManualFallback(t *testing.T) {
	for _, kind := range []string{"manual", "developer", "outside-list", "generation", "nonmember"} {
		t.Run(kind, func(t *testing.T) {
			s := connectFixture(t)
			mode := "auto"
			if kind == "manual" {
				mode = "manual"
			}
			raw, parent := connectIssue(t, s, mode, 0)
			s.mu.Lock()
			p := s.data.Projects["demo"]
			switch kind {
			case "developer":
				p.Members["admin"] = "developer"
				s.data.Projects["demo"] = p
			case "outside-list":
				c := *parent.Connect
				c.AutoProjects = []connectProject{{"other", s.data.Projects["other"].ProjectGenerationID}}
				parent.Connect = &c
				s.api.tokens[parent.ID] = parent
			case "generation":
				c := *parent.Connect
				c.AutoProjects = []connectProject{{"demo", rand.Text()}, {"other", s.data.Projects["other"].ProjectGenerationID}}
				parent.Connect = &c
				s.api.tokens[parent.ID] = parent
			case "nonmember":
				p.Members = map[string]string{}
				s.data.Projects["demo"] = p
			}
			s.mu.Unlock()
			h := New(s, &panelBackend{}, http.NotFoundHandler())
			f := &deviceHTTPFixture{t: t, store: s, h: h}
			w, body := f.rest("POST", directPath+"?project=demo", raw, directForm())
			want := "insufficient_scope"
			if kind == "nonmember" {
				want = "connect_forbidden"
			}
			if w.Code != 403 || body["error"] != want {
				t.Fatalf("direct refusal status=%d error=%v want=%s", w.Code, body["error"], want)
			}
			if len(s.directRates) != 0 {
				t.Fatal("eligibility refusal consumed rate allowance")
			}
			if kind == "nonmember" {
				return
			}
			token, _ := connectHTTPApproved(t, s, "demo", raw, "fallback-child")
			if token.Project != "demo" || !s.Authorize("demo", token.AccessToken) {
				t.Fatal("eligible parent did not reach device approval")
			}
		})
	}
}
