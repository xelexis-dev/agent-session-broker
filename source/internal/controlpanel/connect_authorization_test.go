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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func connectFixture(t *testing.T) *Store {
	t.Helper()
	s := directStore(t)
	if err := s.CreateProject("other"); err != nil {
		t.Fatal(err)
	}
	if err := s.setConnectPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	return s
}
func connectIssue(t *testing.T, s *Store, mode string, cap int) (string, userAPIToken) {
	t.Helper()
	c := connectOptions{AdmissionMode: mode, MaxSessions: cap}
	if mode == "auto" {
		for _, name := range []string{"demo", "other"} {
			c.AutoProjects = append(c.AutoProjects, connectProject{name, s.ProjectGeneration(name)})
		}
	}
	raw, parent, err := s.issueConnectToken("admin", 0, c)
	if err != nil {
		t.Fatal(err)
	}
	return raw, parent
}
func connectPending(t *testing.T, s *Store, project, raw, name string) DeviceResponse {
	t.Helper()
	r := DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: name, InstanceID: "pc-example", RequestID: "request-" + name}
	d, err := s.beginConnectAuthorization(project, raw, r, false)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func connectApproved(t *testing.T, s *Store, project, raw, name string) DeviceTokenResponse {
	t.Helper()
	d := connectPending(t, s, project, raw, name)
	if _, err := s.approvePanelDeviceAuthorization("admin", d.UserCode, name, false); err != nil {
		t.Fatal(err)
	}
	token, err := s.exchangeConnectToken(project, raw, d.DeviceCode, "asb-client-v1", false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range s.connect.audits {
		if e.Kind == "grant-issued" && e.GrantID == token.GrantID && e.Reason == "approved" && e.ActorKind == "user" && e.ActorID == "admin" {
			found = true
		}
	}
	if !found {
		t.Fatal("human approver missing from connect audit")
	}
	return token
}

func TestConnectTokenContractAndAuthentication(t *testing.T) {
	s := connectFixture(t)
	before, _ := json.Marshal(s.data)
	raw, parent := connectIssue(t, s, "manual", 0)
	after, _ := json.Marshal(s.data)
	if !bytes.Equal(before, after) {
		t.Fatal("issuance changed control JSON")
	}
	if parent.ExpiresAt.Sub(parent.CreatedAt) != 180*24*time.Hour || parent.Connect.MaxSessions != 32 {
		t.Fatal("manual defaults")
	}
	rows, _ := s.listRecords()
	if !validAPIToken(parent, parent.key()) || !bytes.Contains(rows[parent.key()], []byte(`"connect":`)) {
		t.Fatal("contract not stored")
	}
	if _, err := s.authenticateConnect(raw); err != nil {
		t.Fatal("connect depends on admin API switch", err)
	}
	if err := s.SetAdminAPI("admin", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authenticateAPI(raw); !errors.Is(err, errAPIUnauthorized) {
		t.Fatal("connect authenticates management API", err)
	}
	if _, _, err := s.IssueOwnUserToken("admin", s.data.Users["admin"].Revision, "mixed", 3600, []string{"connect", "read"}, "", false); !errors.Is(err, ErrInvalid) {
		t.Fatal("mixed issuance accepted")
	}
	mixed := cloneConnectToken(parent)
	mixed.Scopes = append(mixed.Scopes, "read")
	if validAPIToken(mixed, mixed.key()) {
		t.Fatal("mixed stored token accepted")
	}
	management := cloneConnectToken(parent)
	management.Scopes = []string{"read"}
	if validAPIToken(management, management.key()) {
		t.Fatal("management row carries connect metadata")
	}
	autoRaw, auto := connectIssue(t, s, "auto", 0)
	if auto.ExpiresAt.Sub(auto.CreatedAt) != 30*24*time.Hour || auto.Connect.MaxSessions != 8 {
		t.Fatal("auto defaults")
	}
	if _, err := s.authenticateConnect(autoRaw); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode  string
		days  int
		valid bool
	}{{"manual", 365, true}, {"manual", 366, false}, {"auto", 90, true}, {"auto", 91, false}} {
		c := connectOptions{AdmissionMode: tc.mode}
		_, _, err := s.issueConnectToken("admin", int64(tc.days*24*3600), c)
		if (err == nil) != tc.valid {
			t.Fatalf("duration mode=%s days=%d: %v", tc.mode, tc.days, err)
		}
	}
	if _, err := s.BeginDeviceAuthorization("demo", raw, DeviceRequest{}); err == nil {
		t.Fatal("public project endpoint accepted connect")
	}

	t.Run("parent-rate-shared", func(t *testing.T) {
		s := connectFixture(t)
		raw, _ := connectIssue(t, s, "manual", 0)
		for i := 0; i < 20; i++ {
			name := "demo"
			if i%2 == 1 {
				name = "other"
			}
			if _, err := s.beginConnectAuthorization(name, raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: fmt.Sprint(i), InstanceID: "shared-rate-fixture", RequestID: fmt.Sprint(i)}, false); err != nil {
				t.Fatal("early shared rate denial", err)
			}
		}
		if _, err := s.beginConnectAuthorization("other", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "last", InstanceID: "shared-rate-fixture", RequestID: "last"}, false); !errors.Is(err, errDirectRate) {
			t.Fatal("parent rate was per project", err)
		}
	})
}

func TestConnectAggregateManualAutoRace(t *testing.T) {
	s := connectFixture(t)
	raw, parent := connectIssue(t, s, "auto", 1)
	d := connectPending(t, s, "demo", raw, "manual")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := s.approvePanelDeviceAuthorization("admin", d.UserCode, "manual", false)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := s.exchangeConnectDirect("other", raw, directRequest(directForm()), DirectProof{}, "test", false)
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if !errors.Is(err, errConnectCapacity) {
			t.Fatal(err)
		}
	}
	if successes != 1 || connectCount(s.data, parent.ID, s.now()) != 1 {
		t.Fatal("cross-project cap oversubscribed")
	}
	// The same ID in a grant and reservation consumes only one slot.
	s.mu.Lock()
	err := s.connectCapacityLocked(s.data, s.api.tokens)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	p := s.data.Projects["other"]
	p.Tokens = slices.Clone(p.Tokens)
	p.Tokens = append(p.Tokens, SessionToken{ScopeVersion: access.ScopeVersion, Scopes: access.LegacyV1Scopes(), Kind: tokenSession, ParentID: parent.ID, ID: rand.Text() + rand.Text(), OwnerID: "admin", Nickname: "over-budget", IssuedAt: s.now(), ExpiresAt: s.now().Add(sessionLifetime), BoundIdentity: `["codex","other","pc"]`, TokenHash: digest(rand.Text())})
	err = s.commitProject(p)
	s.mu.Unlock()
	if !errors.Is(err, errConnectCapacity) || connectCount(s.data, parent.ID, s.now()) != 1 {
		t.Fatal("commit skipped aggregate recheck", err)
	}
}

func TestConnectRevokeAfterApprovalAndStream(t *testing.T) {
	s := connectFixture(t)
	raw, parent := connectIssue(t, s, "manual", 0)
	live := connectApproved(t, s, "demo", raw, "live")
	grant, _, ok := s.authorize("demo", live.AccessToken)
	if !ok {
		t.Fatal("live child denied")
	}
	d := connectPending(t, s, "other", raw, "pending")
	if _, err := s.approvePanelDeviceAuthorization("admin", d.UserCode, "pending", false); err != nil {
		t.Fatal(err)
	}
	entered, done := make(chan struct{}), make(chan struct{})
	h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "https://broker/mcp?project=demo", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+live.AccessToken)
	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), req) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("stream cleanup timed out")
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not open")
	}
	if err := s.RevokeOwnUserToken("admin", parent.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("long open stream survived parent revoke")
	}
	select {
	case <-grant.Revoked:
	default:
		t.Fatal("open stream not cancelled")
	}
	if s.Authorize("demo", live.AccessToken) {
		t.Fatal("child survived revoke")
	}
	if _, err := s.exchangeConnectToken("other", raw, d.DeviceCode, "asb-client-v1", false); err == nil {
		t.Fatal("revoked parent exchanged approved grant")
	}
	for _, p := range s.data.Projects {
		for _, g := range p.Tokens {
			if g.ParentID == parent.ID && !g.Revoked {
				t.Fatal("child not permanently revoked")
			}
		}
	}
}

func TestConnectTemporarySwitchAndProjectDisable(t *testing.T) {
	s := connectFixture(t)
	raw, _ := connectIssue(t, s, "auto", 0)
	token, err := s.exchangeConnectDirect("demo", raw, directRequest(directForm()), DirectProof{}, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	grant, _, ok := s.authorize("demo", token.AccessToken)
	if !ok {
		t.Fatal("initial authorization")
	}
	if err = s.setConnectPolicy("admin", false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-grant.Revoked:
	default:
		t.Fatal("OFF did not cancel stream")
	}
	if s.Authorize("demo", token.AccessToken) {
		t.Fatal("OFF accepted")
	}
	if err = s.setConnectPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	if !s.Authorize("demo", token.AccessToken) {
		t.Fatal("ON did not restore temporary denial")
	}
	if err = s.SaveProjectFor("admin", "demo", "token", false); err != nil {
		t.Fatal(err)
	}
	if s.Authorize("demo", token.AccessToken) {
		t.Fatal("disabled project accepted")
	}
	if err = s.SaveProjectFor("admin", "demo", "token", true); err != nil {
		t.Fatal(err)
	}
	if !s.Authorize("demo", token.AccessToken) {
		t.Fatal("project reactivation revoked child")
	}
}

func TestConnectExplicitMembershipAndPermanentDemotion(t *testing.T) {
	s := connectFixture(t)
	if err := s.CreateUser("admin", "owner", testPassword, "user"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectMember("admin", "demo", "owner", "owner"); err != nil {
		t.Fatal(err)
	}
	raw, parent := connectIssue(t, s, "auto", 0)
	token := connectApproved(t, s, "demo", raw, "before")
	if err := s.SetProjectMember("admin", "demo", "admin", "developer"); err != nil {
		t.Fatal(err)
	}
	if s.Authorize("demo", token.AccessToken) {
		t.Fatal("demotion retained grant")
	}
	if slices.Contains(s.api.tokens[parent.ID].Connect.AutoProjects, connectProject{"demo", s.ProjectGeneration("demo")}) {
		t.Fatal("demotion retained auto project")
	}
	if _, err := s.exchangeConnectDirect("demo", raw, directRequest(directForm()), DirectProof{}, "test", false); err == nil {
		t.Fatal("explicit Developer auto registered")
	}
	// Developer still needs approval, even though the account is a system admin.
	d := connectPending(t, s, "demo", raw, "developer")
	if _, err := s.exchangeConnectToken("demo", raw, d.DeviceCode, "asb-client-v1", false); !errors.Is(err, errDevicePending) {
		t.Fatal("Developer bypassed approval", err)
	}
	if err := s.SetProjectMember("admin", "demo", "admin", "owner"); err != nil {
		t.Fatal(err)
	}
	if s.Authorize("demo", token.AccessToken) {
		t.Fatal("role restoration resurrected child")
	}
	if _, err := s.exchangeConnectDirect("demo", raw, directRequest(directForm()), DirectProof{}, "test", false); err == nil {
		t.Fatal("role restoration resurrected auto eligibility")
	}
	if err := s.SetProjectMember("admin", "demo", "admin", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.beginConnectAuthorization("demo", raw, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: "implicit", InstanceID: "pc", RequestID: "implicit"}, false); err == nil {
		t.Fatal("implicit admin Owner connected")
	}
	found := false
	for _, e := range s.connect.audits {
		if e.Kind == "auto-project-removed" && e.Reason == "membership-lost" {
			found = true
		}
	}
	if !found {
		t.Fatal("auto removal audit missing")
	}
}

func TestConnectDeletionGenerationAndRevokeAll(t *testing.T) {
	s := connectFixture(t)
	raw, parent := connectIssue(t, s, "auto", 0)
	token := connectApproved(t, s, "demo", raw, "old")
	oldGen := s.ProjectGeneration("demo")
	if _, err := s.beginProjectDeletion("admin", "demo", "demo"); err != nil {
		t.Fatal(err)
	}
	if err := s.finishProjectDeletion("demo"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("demo"); err != nil {
		t.Fatal(err)
	}
	if s.ProjectGeneration("demo") == oldGen || s.Authorize("demo", token.AccessToken) {
		t.Fatal("generation reused or child resurrected")
	}
	if _, err := s.exchangeConnectDirect("demo", raw, directRequest(directForm()), DirectProof{}, "test", false); err == nil {
		t.Fatal("recreated project auto allowed")
	}
	if len(s.api.tokens[parent.ID].Connect.AutoProjects) != 1 {
		t.Fatal("other project auto eligibility changed")
	}
	if err := s.RevokeOwnUserTokens("admin"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.listRecords()
	if _, ok := rows[parent.key()]; ok {
		t.Fatal("revoke-all retained connect row")
	}
}

func TestConnectBatchFailureAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if s.Setup(testPassword) != nil || s.CreateProject("demo") != nil || s.CreateProject("other") != nil || s.setConnectPolicy("admin", true) != nil {
		t.Fatal("setup")
	}
	raw, parent := connectIssue(t, s, "manual", 0)
	token := connectApproved(t, s, "demo", raw, "atomic")
	grant, _, ok := s.authorize("demo", token.AccessToken)
	if !ok {
		t.Fatal("initial grant")
	}
	before, _ := s.listRecords()
	batch := s.writeBatch
	s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	if err = s.RevokeOwnUserToken("admin", parent.ID); !errors.Is(err, ErrStorage) {
		t.Fatal("missing batch failure")
	}
	select {
	case <-grant.Revoked:
		t.Fatal("notified before commit")
	default:
	}
	after, _ := s.listRecords()
	if !maps.EqualFunc(before, after, bytes.Equal) {
		t.Fatal("failed batch changed durable rows")
	}
	s.writeBatch = batch
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Authorize("demo", token.AccessToken) {
		t.Fatal("failed revoke corrupted restart")
	}
	if err = s.RevokeOwnUserToken("admin", parent.ID); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Authorize("demo", token.AccessToken) {
		t.Fatal("revocation lost on restart")
	}
}

func TestConnectExpiryRevisionAndAuditSaturation(t *testing.T) {
	for _, kind := range []string{"expiry", "revision", "audit"} {
		t.Run(kind, func(t *testing.T) {
			s := connectFixture(t)
			raw, parent := connectIssue(t, s, "manual", 0)
			token := connectApproved(t, s, "demo", raw, "child")
			switch kind {
			case "expiry":
				future := parent.ExpiresAt.Add(time.Second)
				s.now = func() time.Time { return future }
				if err := s.SweepExpired(); err != nil {
					t.Fatal(err)
				}
			case "revision":
				if err := s.UpdateUser("admin", "admin", "admin", true, "replacement-password"); err != nil {
					t.Fatal(err)
				}
			case "audit":
				puts := map[string][]byte{}
				for i := 0; i < maxConnectAudit; i++ {
					e := connectEvent(parent, Project{}, "", "token-issued", "explicit", "admin")
					e.At = s.now().Add(-time.Hour)
					key := fmt.Sprintf("connect-audit/synthetic-%05d", i)
					s.connect.audits[key] = e
					puts[key], _ = json.Marshal(e)
				}
				if err := s.writeBatch(puts, nil); err != nil {
					t.Fatal(err)
				}
				if err := s.RevokeOwnUserToken("admin", parent.ID); err != nil {
					t.Fatal("saturated audit blocked revoke", err)
				}
				if len(s.connect.audits) > maxConnectAudit {
					t.Fatal("audit cap exceeded")
				}
			}
			if s.Authorize("demo", token.AccessToken) {
				t.Fatal("permanent revocation failed")
			}
			rows, _ := s.listRecords()
			if _, ok := rows[parent.key()]; ok {
				t.Fatal("revoked parent row remains")
			}
		})
	}

	t.Run("short-parent-clamps-child", func(t *testing.T) {
		s := connectFixture(t)
		raw, parent, err := s.issueConnectToken("admin", 300, connectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		token := connectApproved(t, s, "demo", raw, "short")
		grant, _, ok := s.authorize("demo", token.AccessToken)
		if !ok || !grant.ExpiresAt.Equal(parent.ExpiresAt) {
			t.Fatal("grant outlives parent")
		}
	})

	t.Run("reservation-expiry-audit", func(t *testing.T) {
		s := connectFixture(t)
		raw, parent := connectIssue(t, s, "auto", 0)
		token, err := s.exchangeConnectDirect("demo", raw, directRequest(directForm()), DirectProof{}, "test", false)
		if err != nil {
			t.Fatal(err)
		}
		future := s.now().Add(directWindow + time.Second)
		s.now = func() time.Time { return future }
		if err = s.SweepExpired(); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range s.connect.audits {
			if e.ParentID == parent.ID && e.GrantID == token.GrantID && e.Kind == "grant-revoked" && e.Reason == "expired" {
				found = true
			}
		}
		if !found || s.Authorize("demo", token.AccessToken) {
			t.Fatal("reservation expiry missing revocation or audit")
		}
	})
}

func TestConnectAmbiguousParentAndScopeEditing(t *testing.T) {
	s := connectFixture(t)
	raw, parent := connectIssue(t, s, "manual", 0)
	token := connectApproved(t, s, "demo", raw, "child")
	if _, err := s.TokenScopeEditFor("admin", "demo", token.GrantID); err == nil {
		t.Fatal("user-parent scopes editable")
	}
	s.mu.Lock()
	p := s.data.Projects["demo"]
	p.Tokens = append(slices.Clone(p.Tokens), SessionToken{Kind: tokenProject, ScopeVersion: access.ScopeVersion, Scopes: access.LegacyV1Scopes(), ID: parent.ID, OwnerID: "admin", IssuedAt: s.now(), TokenHash: digest(rand.Text())})
	err := s.commitProject(p)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if s.Authorize("demo", token.AccessToken) {
		t.Fatal("ambiguous parent accepted")
	}
}

func TestConnectSettingFailsClosed(t *testing.T) {
	s := connectFixture(t)
	for _, raw := range []string{"", `{}`, `{"v":2,"enabled":true}`, `{"v":1,"enabled":null}`, `{"v":1,"enabled":true,"enabled":false}`, `{"v":1,"enabled":"true"}`} {
		s.loadConnect(map[string][]byte{connectSettingKey: []byte(raw)})
		if s.connect.enabled || s.connect.diagnostic != connectSettingDiagnostic {
			t.Fatalf("malformed setting accepted: %s", raw)
		}
	}
}
