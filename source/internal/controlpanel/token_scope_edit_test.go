package controlpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func scopeEditParent(t *testing.T, s *Store, raw string) SessionToken {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.data.Projects {
		for _, token := range p.Tokens {
			if token.TokenHash == digest(raw) {
				return token
			}
		}
	}
	t.Fatal("parent fixture missing")
	return SessionToken{}
}
func scopeEditVersion(s *Store, parent SessionToken) string {
	return settingsVersion("token-scopes", "demo", append([]string{s.ProjectGeneration("demo"), parent.ID}, parent.Scopes...)...)
}
func TestTokenScopeEditAtomicChildrenAndWatchIsolation(t *testing.T) {
	t.Parallel()
	s := directStore(t)
	parentRaw := directParent(t, s, 8)
	parent := scopeEditParent(t, s, parentRaw)
	child := churnCredentialFor(t, s, parentRaw, 401, true)
	otherParent := directParent(t, s, 8)
	other := churnCredentialFor(t, s, otherParent, 402, true)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	browser := loginPanelUser(t, h, "admin")
	scopes := []string{access.MessageSend, access.SessionRegister}
	response := browser.call("POST", "/tokens/scopes", url.Values{"project": {"demo"}, "id": {parent.ID}, "scope": scopes, "settings_version": {scopeEditVersion(s, parent)}})
	if response.Code != 303 {
		t.Fatalf("scope edit returned %d, want successful atomic mutation", response.Code)
	}
	p := s.data.Projects["demo"]
	found := false
	for _, token := range p.Tokens {
		if token.ParentID == parent.ID && token.Kind == tokenSession {
			found = true
			if !slices.Equal(token.Scopes, scopes) {
				t.Error("usable child did not inherit new delegated scopes")
			}
		}
	}
	if !found {
		t.Fatal("child missing")
	}
	select {
	case <-child.grant.Revoked:
	default:
		t.Error("changed child stream not interrupted")
	}
	select {
	case <-child.changes:
		t.Error("whole project generation interrupted")
	default:
	}
	select {
	case <-other.grant.Revoked:
		t.Error("unrelated parent stream interrupted")
	default:
	}
}

func TestTokenScopeEditRoleAndIssuanceMatrix(t *testing.T) {
	t.Parallel()
	for _, actor := range []string{"admin", "owner", "developer", "developer-other", "viewer", "outsider"} {
		for _, ready := range []bool{false, true} {
			for _, change := range []string{"narrow", "widen", "mixed"} {
				t.Run(fmt.Sprintf("%s/%t/%s", actor, ready, change), func(t *testing.T) {
					t.Parallel()
					s := userStore(t)
					s.data.ScopeIssuanceReady = true
					issuer := "developer"
					caller := actor
					if actor == "developer-other" {
						issuer = "owner"
						caller = "developer"
					}
					initial := []string{access.MessageReceive, access.MessageSend, access.SessionRegister}
					raw, err := s.IssueProjectTokenWithOptionsFor(issuer, "demo", "matrix", ProjectTokenOptions{Scopes: initial})
					if err != nil {
						t.Fatal("fixture issuance")
					}
					parent := scopeEditParent(t, s, raw)
					s.data.ScopeIssuanceReady = policyBool(ready)
					next := []string{access.MessageSend, access.SessionRegister}
					if change == "widen" {
						next = append([]string{access.MemoryRead}, initial...)
					}
					if change == "mixed" {
						next = append([]string{access.MemoryRead}, next...)
					}
					_, err = s.EditTokenScopesFor(caller, "demo", parent.ID, next, []string{scopeEditVersion(s, parent)})
					allowed := actor == "owner" || actor == "admin" || actor == "developer" && change == "narrow"
					if change != "narrow" && !ready {
						allowed = false
					}
					if (err == nil) != allowed {
						t.Fatalf("allowed=%t, error=%v", allowed, err)
					}
				})
			}
		}
	}
}

func TestTokenScopeEditFailureNoopAndVersion(t *testing.T) {
	t.Parallel()
	s := directStore(t)
	raw := directParent(t, s, 8)
	parent := scopeEditParent(t, s, raw)
	child := churnCredentialFor(t, s, raw, 410, true)
	version := scopeEditVersion(s, parent)
	before, _ := json.Marshal(s.data)
	audits := len(s.audits)
	noauto := access.DelegatedScopes(parent.Scopes)
	if _, err := s.EditTokenScopesFor("admin", "demo", parent.ID, noauto, []string{version}); err != nil {
		t.Fatal(err)
	}
	if len(s.audits) != audits {
		t.Fatal("no-op audited")
	}
	write := s.writeBatch
	s.writeBatch = func(map[string][]byte, []string) error { return errors.New("synthetic write failure") }
	scopes := []string{access.MessageSend, access.SessionRegister}
	if _, err := s.EditTokenScopesFor("admin", "demo", parent.ID, scopes, []string{version}); !errors.Is(err, ErrStorage) {
		t.Fatal("write failure not propagated")
	}
	after, _ := json.Marshal(s.data)
	if !bytes.Equal(before, after) || len(s.audits) != audits {
		t.Fatal("failed write published state/audit")
	}
	select {
	case <-child.grant.Revoked:
		t.Fatal("no-op/failed write cancelled stream")
	default:
	}
	s.writeBatch = write
	saved, err := s.EditTokenScopesFor("admin", "demo", parent.ID, scopes, []string{version})
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := s.EditTokenScopesFor("admin", "demo", parent.ID, scopes, []string{version})
	if !errors.Is(err, errSettingsConflict) || conflict.Version != saved.Version || !slices.Equal(conflict.Scopes, saved.Scopes) {
		t.Fatal("two-tab conflict lost current snapshot")
	}
	for _, v := range [][]string{nil, {""}, {"bad"}, {version, version}, {strings.Repeat("Z", 64)}} {
		if _, err := s.EditTokenScopesFor("admin", "demo", parent.ID, scopes, v); !errors.Is(err, ErrInvalid) {
			t.Fatal("missing/malformed version accepted")
		}
	}
}

func TestTokenScopeEditTerminalIntersectionAndRestart(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"revoked", "expired"} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", terminal, empty), func(t *testing.T) {
				t.Parallel()
				s := directStore(t)
				raw := directParent(t, s, 8)
				parent := scopeEditParent(t, s, raw)
				child := churnCredentialFor(t, s, raw, 420, true)
				p := s.data.Projects["demo"]
				for i, token := range p.Tokens {
					if token.ParentID == parent.ID {
						if terminal == "revoked" {
							p.Tokens[i].Revoked = true
						} else {
							p.Tokens[i].ExpiresAt = s.now().Add(-time.Second)
						}
						if empty {
							p.Tokens[i].Scopes = []string{access.MessageReceive}
						}
					}
				}
				if err := s.commitProject(p); err != nil {
					t.Fatal("terminal fixture")
				}
				before, _ := json.Marshal(s.data)
				audits := len(s.audits)
				next := []string{access.MessageSend, access.SessionRegister}
				_, err := s.EditTokenScopesFor("admin", "demo", parent.ID, next, []string{scopeEditVersion(s, parent)})
				if empty {
					after, _ := json.Marshal(s.data)
					if !errors.Is(err, errTokenScopeHistory) || !bytes.Equal(before, after) || len(s.audits) != audits {
						t.Fatal("empty intersection not atomically rejected")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					for _, token := range s.data.Projects["demo"].Tokens {
						if token.ParentID == parent.ID && (tokenActive(token, s.now()) || !slices.Equal(token.Scopes, next)) {
							t.Fatal("terminal token widened or revived")
						}
					}
				}
				_ = child
				directory := t.TempDir()
				data, _ := json.Marshal(s.data)
				var restarted state
				if json.Unmarshal(data, &restarted) != nil || !validState(restarted) || !validScopeState(restarted) {
					t.Fatal("edited state fails restart validation")
				}
				clone, err := OpenSQLite(directory)
				if err != nil {
					t.Fatal(err)
				}
				if err := clone.commit(s.data); err != nil {
					t.Fatal(err)
				}
				if err := clone.Close(); err != nil {
					t.Fatal(err)
				}
				clone, err = OpenSQLite(directory)
				if err != nil {
					t.Fatal("restart rejected retained history")
				}
				clone.Close()
			})
		}
	}
}

func TestTokenScopeEditDevicesAndReplay(t *testing.T) {
	t.Parallel()
	s := directStore(t)
	raw := directParent(t, s, 8)
	parent := scopeEditParent(t, s, raw)
	req := DeviceRequest{ClientID: "asb-client-v1", InstanceID: "scope-edit-device", Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111431", RequestID: "scope-device-request"}
	pending, err := s.BeginDeviceAuthorization("demo", raw, req)
	if err != nil {
		t.Fatal(err)
	}
	approvedReq := req
	approvedReq.SessionID = "11111111-1111-4111-8111-111111111432"
	approvedReq.RequestID = "scope-device-approved"
	approved, err := s.BeginDeviceAuthorization("demo", raw, approvedReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveDeviceAuthorization("admin", approved.UserCode, "approved-agent", false); err != nil {
		t.Fatal(err)
	}
	next := []string{access.MessageSend, access.SessionRegister}
	if _, err := s.EditTokenScopesFor("admin", "demo", parent.ID, next, []string{scopeEditVersion(s, parent)}); err != nil {
		t.Fatal(err)
	}
	widened := []string{access.MessageReceive, access.MessageSend, access.SessionRegister}
	current := scopeEditParent(t, s, raw)
	if _, err := s.EditTokenScopesFor("admin", "demo", parent.ID, widened, []string{scopeEditVersion(s, current)}); err != nil {
		t.Fatal(err)
	}
	for _, d := range s.data.Projects["demo"].Devices {
		want := next
		if d.Status == "pending" {
			want = widened
		}
		if !slices.Equal(d.Scopes, want) {
			t.Fatal("device snapshot classification incorrect")
		}
	}
	if _, err := s.ApproveDeviceAuthorization("admin", pending.UserCode, "pending-agent", false); err != nil {
		t.Fatal(err)
	}
	for _, device := range []DeviceResponse{pending, approved} {
		grant, err := s.ExchangeDeviceToken("demo", raw, device.DeviceCode, req.ClientID, false)
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.devicePoll = nil
		s.mu.Unlock()
		retry, err := s.ExchangeDeviceToken("demo", raw, device.DeviceCode, req.ClientID, false)
		if err != nil || retry.AccessToken == grant.AccessToken {
			t.Fatal("lost-response re-exchange failed")
		}
		g, _, ok := s.authorize("demo", retry.AccessToken)
		if !ok || !slices.Equal(g.Scopes, widened) {
			t.Fatal("approval/exchange minted stale scopes")
		}
	}
	// Direct replay takes scopes from its updated child, not the historical device snapshot.
	direct := directRequest(directForm())
	direct.RequestID = "scope-direct-replay"
	direct.SessionID = "11111111-1111-4111-8111-111111111433"
	first, err := s.ExchangeDirect("demo", raw, direct, DirectProof{}, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ExchangeDirect("demo", raw, direct, DirectProof{}, "fixture", false)
	if err != nil || !slices.Equal(first.Scopes, replay.Scopes) {
		t.Fatal("direct replay changed effective scopes")
	}
}

func TestTokenScopeEditHTTPValidationAndIsolation(t *testing.T) {
	t.Parallel()
	s := userStore(t)
	s.data.ScopeIssuanceReady = true
	raw, err := s.IssueProjectTokenFor("owner", "demo", "HTTP")
	if err != nil {
		t.Fatal(err)
	}
	parent := scopeEditParent(t, s, raw)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	form := url.Values{"project": {"demo"}, "id": {parent.ID}, "scope": {access.MessageSend, access.SessionRegister}, "settings_version": {scopeEditVersion(s, parent)}}
	for _, key := range []string{"auto_cap", "auto_days", "owner", "admission_mode", "unknown"} {
		bad := cloneScopeForm(form)
		bad.Set(key, "injected")
		if owner.call("POST", "/tokens/scopes", bad).Code != 400 {
			t.Error("injected admission field accepted")
		}
	}
	for _, scope := range []string{access.SessionRegisterAuto, "unknown", access.SessionRegister} {
		bad := cloneScopeForm(form)
		bad["scope"] = append(bad["scope"], scope)
		if owner.call("POST", "/tokens/scopes", bad).Code != 400 {
			t.Error("auto/unknown/duplicate scope accepted")
		}
	}
	for _, v := range [][]string{nil, {""}, {"invalid"}, {form.Get("settings_version"), form.Get("settings_version")}} {
		bad := cloneScopeForm(form)
		bad["settings_version"] = v
		if owner.call("POST", "/tokens/scopes", bad).Code != 400 {
			t.Error("missing/malformed/repeated version accepted")
		}
	}
	developer := loginPanelUser(t, h, "developer")
	if developer.call("POST", "/tokens/scopes", form).Code != 403 {
		t.Error("IDOR accepted")
	}
	req := httptest.NewRequest("POST", "/tokens/scopes", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(owner.cookie)
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, req)
	if denied.Code != 403 {
		t.Error("CSRF accepted")
	}
	if owner.call("GET", "/tokens/scopes", nil).Code != 405 {
		t.Error("unexpected editor GET exposed")
	}
	if owner.call("POST", "/tokens/scopes", form).Code != 303 {
		t.Fatal("valid edit rejected")
	}
	conflict := owner.call("POST", "/tokens/scopes", form)
	current, err := s.TokenScopeEditFor("owner", "demo", parent.ID)
	if err != nil || conflict.Code != 409 || !strings.Contains(conflict.Body.String(), current.Version) || current.Version == form.Get("settings_version") || !strings.Contains(conflict.Body.String(), `id="scope-editor"`) {
		t.Fatal("409 omitted current scopes/fresh version")
	}
}
func cloneScopeForm(form url.Values) url.Values {
	result := url.Values{}
	for k, v := range form {
		result[k] = slices.Clone(v)
	}
	return result
}

func TestTokenScopeEditTerminalDeviceEmptyIntersection(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"approved", "denied", "expired"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			s := directStore(t)
			raw := directParent(t, s, 8)
			parent := scopeEditParent(t, s, raw)
			req := DeviceRequest{ClientID: "asb-client-v1", InstanceID: "terminal-device", Harness: "codex", SessionID: "terminal-session", RequestID: "terminal-request"}
			response, err := s.BeginDeviceAuthorization("demo", raw, req)
			if err != nil {
				t.Fatal(err)
			}
			if status == "approved" {
				if _, err := s.ApproveDeviceAuthorization("admin", response.UserCode, "terminal-agent", false); err != nil {
					t.Fatal(err)
				}
			}
			p := s.data.Projects["demo"]
			p.Devices[0].Scopes = []string{access.MessageReceive}
			if status == "denied" {
				p.Devices[0].Status = "denied"
			}
			if status == "expired" {
				p.Devices[0].ExpiresAt = s.now().Add(-time.Second)
			}
			if err := s.commitProject(p); err != nil {
				t.Fatal("terminal device fixture")
			}
			before, _ := json.Marshal(s.data)
			auditCount := len(s.audits)
			if _, err := s.EditTokenScopesFor("admin", "demo", parent.ID, []string{access.SessionRegister}, []string{scopeEditVersion(s, parent)}); !errors.Is(err, errTokenScopeHistory) {
				t.Fatal("terminal empty device intersection accepted")
			}
			after, _ := json.Marshal(s.data)
			if !bytes.Equal(before, after) || len(s.audits) != auditCount {
				t.Fatal("terminal rejection wrote state/audit")
			}
		})
	}
}

func TestTokenScopeEditAuditAndOlderReader(t *testing.T) {
	t.Parallel()
	s := directStore(t)
	raw := directParent(t, s, 8)
	parent := scopeEditParent(t, s, raw)
	_ = churnCredentialFor(t, s, raw, 440, true)
	if _, err := s.EditTokenScopesFor("admin", "demo", parent.ID, []string{access.MessageSend, access.SessionRegister}, []string{scopeEditVersion(s, parent)}); err != nil {
		t.Fatal(err)
	}
	var event AuditEvent
	for _, e := range s.audits {
		if e.Kind == tokenScopesAuditKind {
			event = e
		}
	}
	if !validAudit(event) || event.TokenScopes.ActorID != "admin" || event.OwnerID != parent.OwnerID || event.ParentID != parent.ID || event.TokenScopes.Label != parent.Label || event.TokenScopes.ChangedChildren != 1 || event.ProjectGenerationID != s.ProjectGeneration("demo") {
		t.Fatal("audit missing authenticated metadata/actual child count")
	}
	// Recreate the previous strict reader's field set, which had no tokenScopes member.
	directory := s.root.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSQLite(directory)
	if err != nil {
		t.Fatal("new audit failed restart")
	}
	defer restarted.Close()
	found := false
	for _, e := range restarted.audits {
		if e.Kind == tokenScopesAuditKind {
			found = true
		}
	}
	if !found {
		t.Fatal("scope audit lost after restart")
	}
	current := reflect.TypeOf(AuditEvent{})
	var oldFields []reflect.StructField
	for i := 0; i < current.NumField(); i++ {
		field := current.Field(i)
		if field.Name != "TokenScopes" {
			oldFields = append(oldFields, field)
		}
	}
	legacy := reflect.New(reflect.StructOf(oldFields)).Interface()
	rawAudit, _ := json.Marshal(event)
	decoder := json.NewDecoder(bytes.NewReader(rawAudit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(legacy); err == nil {
		t.Fatal("older strict reader unexpectedly accepted new audit member")
	}
}

func TestTokenScopeEditApprovalPollAndDirectReplayRace(t *testing.T) {
	t.Parallel()
	for iteration := 0; iteration < 8; iteration++ {
		s := directStore(t)
		raw := directParent(t, s, 8)
		parent := scopeEditParent(t, s, raw)
		req := DeviceRequest{ClientID: "asb-client-v1", InstanceID: "race-device", Harness: "codex", SessionID: "race-session", RequestID: "race-request"}
		device, err := s.BeginDeviceAuthorization("demo", raw, req)
		if err != nil {
			t.Fatal(err)
		}
		direct := directRequest(directForm())
		direct.RequestID = "race-direct"
		next := []string{access.MessageSend, access.SessionRegister}
		version := scopeEditVersion(s, parent)
		original := s.writeBatch
		s.writeBatch = func(puts map[string][]byte, deletes []string) error {
			for key, value := range puts {
				if !strings.HasPrefix(key, "audit/") {
					continue
				}
				var event AuditEvent
				if json.Unmarshal(value, &event) != nil || event.Kind != tokenScopesAuditKind {
					continue
				}
				var candidate state
				if json.Unmarshal(puts["state"], &candidate) != nil {
					t.Error("candidate decode")
				}
				changed := 0
				for _, before := range s.data.Projects["demo"].Tokens {
					if before.ParentID != parent.ID || !s.sessionTokenEligibleLocked(s.data.Projects["demo"], before) {
						continue
					}
					for _, after := range candidate.Projects["demo"].Tokens {
						if after.ID == before.ID && !slices.Equal(before.Scopes, after.Scopes) {
							changed++
						}
					}
				}
				if event.TokenScopes.ChangedChildren != changed {
					t.Error("audit used a stale preview child count")
				}
			}
			return original(puts, deletes)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, operation := range []func(){
			func() {
				if _, err := s.EditTokenScopesFor("admin", "demo", parent.ID, next, []string{version}); err != nil {
					t.Error("racing edit failed")
				}
			},
			func() {
				if _, err := s.ApproveDeviceAuthorization("admin", device.UserCode, "race-agent", false); err != nil {
					t.Error("racing approval failed")
				}
			},
			func() {
				_, err := s.ExchangeDeviceToken("demo", raw, device.DeviceCode, req.ClientID, false)
				if err != nil && !errors.Is(err, errDevicePending) {
					t.Error("unexpected poll result")
				}
			},
			func() {
				if _, err := s.ExchangeDirect("demo", raw, direct, DirectProof{}, "fixture", false); err != nil {
					t.Error("racing direct failed")
				}
			},
		} {
			wg.Add(1)
			go func(operation func()) { defer wg.Done(); <-start; operation() }(operation)
		}
		close(start)
		wg.Wait()
		replay, err := s.ExchangeDirect("demo", raw, direct, DirectProof{}, "fixture", false)
		if err != nil || !slices.Equal(replay.Scopes, next) {
			t.Fatal("replay returned stale credential scopes")
		}
		if !validScopeState(s.data) {
			t.Fatal("racing edit created unloadable dependent state")
		}
		for _, child := range s.data.Projects["demo"].Tokens {
			if child.ParentID == parent.ID && !slices.Equal(child.Scopes, next) {
				t.Fatal("racing issuance used stale scopes")
			}
		}
	}
}

func TestTokenScopeEditRevokeGenerationAndRoleRaces(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"revoke", "generation", "role"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			s := userStore(t)
			s.data.ScopeIssuanceReady = true
			raw, err := s.IssueProjectTokenFor("developer", "demo", "race")
			if err != nil {
				t.Fatal(err)
			}
			parent := scopeEditParent(t, s, raw)
			version := scopeEditVersion(s, parent)
			if kind == "role" {
				if err := s.SetProjectMember("owner", "demo", "developer", ""); err != nil {
					t.Fatal(err)
				}
				if _, err := s.EditTokenScopesFor("developer", "demo", parent.ID, []string{access.SessionRegister}, []string{version}); !errors.Is(err, ErrForbidden) {
					t.Fatal("removed actor's old form retained authority")
				}
				return
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, err := s.EditTokenScopesFor("owner", "demo", parent.ID, []string{access.SessionRegister}, []string{version})
				if err != nil && !errors.Is(err, ErrForbidden) {
					t.Error("unexpected edit race result")
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				if kind == "revoke" {
					if err := s.RevokeTokenFor("owner", "demo", parent.ID); err != nil {
						t.Error("revoke failed")
					}
				} else {
					idle, err := s.beginProjectDeletion("owner", "demo", "demo")
					if err != nil {
						t.Error("delete failed")
						return
					}
					<-idle
					if s.finishProjectDeletion("demo") != nil || s.CreateProjectFor("owner", "demo") != nil {
						t.Error("recreate failed")
					}
				}
			}()
			close(start)
			wg.Wait()
			if _, err := s.TokenScopeEditFor("owner", "demo", parent.ID); !errors.Is(err, ErrForbidden) {
				t.Fatal("revoked or old-generation parent remained editable")
			}
			if !validScopeState(s.data) {
				t.Fatal("race created invalid persisted state")
			}
		})
	}
}
