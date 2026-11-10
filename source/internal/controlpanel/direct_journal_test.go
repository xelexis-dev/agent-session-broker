package controlpanel

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func directParent(t *testing.T, s *Store, cap int) string {
	t.Helper()
	raw, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", "auto", ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto), MaxAutoBindings: cap})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func directRequest(form url.Values) DirectRequest {
	return DirectRequest{DeviceRequest: DeviceRequest{ClientID: form.Get("client_id"), Harness: form.Get("harness"), SessionID: form.Get("session_id"), InstanceID: form.Get("asb_client_instance_id"), RequestID: form.Get("request_id")}, RequestedNickname: form.Get("requested_nickname")}
}
func TestDirectGateAndDiscovery(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 8)
	s.data.ScopeIssuanceReady = false
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	w := directPOST(t, h, parent, directForm())
	if w.Code != 403 {
		t.Fatal("gate off exchange accepted")
	}
	for _, project := range []string{"demo", "not-present"} {
		req := httptest.NewRequest("GET", "https://broker/oauth/capabilities?project="+project, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 || strings.Contains(w.Body.String(), "direct-registration-v1") {
			t.Fatal("gate off discovery advertised direct")
		}
	}
	if len(s.data.Projects["demo"].AutoBindings) != 0 {
		t.Fatal("gate off reservation created")
	}
	s.data.ScopeIssuanceReady = true
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://broker/oauth/capabilities", nil))
	if !strings.Contains(w.Body.String(), "direct-registration-v1") {
		t.Fatal("enabled direct capability absent")
	}
}
func TestDirectReplayRotationRestartAndAtomicFailure(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 8)
	r := directRequest(directForm())
	first, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	if first.GrantID != second.GrantID || first.AccessToken == second.AccessToken {
		t.Fatal("lost response replay allocated another grant or retained raw credential")
	}
	if _, _, ok := s.authorize("demo", first.AccessToken); ok {
		t.Fatal("old unused response credential still accepted")
	}
	p := s.data.Projects["demo"]
	if len(p.AutoBindings) != 1 || len(p.AutoExchanges) != 1 || len(p.Tokens) != 2 {
		t.Fatal("replay added reservation or grant")
	}
	r.RequestedNickname = "changed"
	if _, err = s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false); !errors.Is(err, errDirectConflict) {
		t.Fatal("request nickname replay accepted")
	}
	r = directRequest(directForm())
	dir := s.root.Name()
	s.Close()
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.data.ScopeIssuanceReady = true
	third, err := reopened.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
	if err != nil || third.GrantID != first.GrantID {
		t.Fatal("restart replay lost allocation")
	}
	audit, err := reopened.DirectAuditFor("admin", "demo")
	if err != nil || len(audit) < 3 {
		t.Fatal("audit did not survive restart")
	}
	raw, _ := json.Marshal(audit)
	for _, secret := range []string{first.AccessToken, second.AccessToken, third.AccessToken, parent, r.identity()} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("secret/raw identity persisted in audit")
		}
	}
	before, _ := json.Marshal(reopened.data)
	auditCount := len(reopened.audits)
	write := reopened.writeBatch
	reopened.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	r.SessionID = "22222222-2222-4222-8222-222222222222"
	r.RequestID = "failure"
	if _, err = reopened.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false); !errors.Is(err, ErrStorage) {
		t.Fatal("batch failure not returned")
	}
	after, _ := json.Marshal(reopened.data)
	if string(before) != string(after) || len(reopened.audits) != auditCount {
		t.Fatal("failed batch published partial state")
	}
	reopened.writeBatch = write
}
func TestDirectConcurrentIdentityCap(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 1)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 1; i <= 2; i++ {
		i := i
		wg.Go(func() {
			r := directRequest(directForm())
			r.RequestID = fmt.Sprintf("cap-%d", i)
			r.SessionID = fmt.Sprintf("%08d-1111-4111-8111-111111111111", i)
			_, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	success, denied := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, errDirectCapacity) {
			denied++
		} else {
			t.Fatal("unexpected cap result")
		}
	}
	if success != 1 || denied != 1 || len(s.data.Projects["demo"].AutoBindings) != 1 || len(s.data.Projects["demo"].Tokens) != 2 {
		t.Fatal("concurrent cap escaped")
	}
}
func TestDirectNicknameFallbackAndSecurityRejection(t *testing.T) {
	for _, name := range []string{"", "has space", "occupied", "BROKER-ADMIN", "name:worker", "registration:ABC", "session:abc"} {
		t.Run(name, func(t *testing.T) {
			s := directStore(t)
			parent := directParent(t, s, 8)
			r := directRequest(directForm())
			r.RequestedNickname = name
			proof := DirectProof{Names: []string{"occupied"}}
			out, err := s.ExchangeDirect("demo", parent, r, proof, "peer", false)
			security := strings.EqualFold(name, "broker-admin") || typedDirectName(name)
			if security {
				if !errors.Is(err, errDirectConflict) || len(s.data.Projects["demo"].AutoBindings) != 0 {
					t.Fatal("reserved name did not fail without reservation")
				}
				return
			}
			if err != nil || !strings.HasPrefix(out.Nickname, "auto-") {
				t.Fatal("nonsecurity name did not fall back")
			}
		})
	}
}
func TestDirectReservationExpiryAndAuditRetention(t *testing.T) {
	s := directStore(t)
	now := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	parent := directParent(t, s, 8)
	if _, err := s.ExchangeDirect("demo", parent, directRequest(directForm()), DirectProof{}, "peer", false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(directWindow)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if len(s.data.Projects["demo"].AutoBindings) != 0 {
		t.Fatal("unclaimed reservation did not expire")
	}
	now = now.Add(auditRetention)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if len(s.audits) != 0 {
		t.Fatal("expired audit did not sweep")
	}
}

func TestDirectRateLimitsAndHTTPShapeFailures(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 64)
	now := time.Now()
	s.now = func() time.Time { return now }
	for i := 0; i < 11; i++ {
		r := directRequest(directForm())
		r.RequestID = fmt.Sprint("rate-", i)
		r.SessionID = fmt.Sprintf("%08d-1111-4111-8111-111111111111", i)
		_, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
		if i < 10 && err != nil {
			t.Fatal(err)
		}
		if i == 10 && !errors.Is(err, errDirectRate) {
			t.Fatal("parent burst not enforced")
		}
	}
	now = now.Add(2 * time.Second)
	r := directRequest(directForm())
	r.RequestID = "refilled"
	r.SessionID = "99999999-1111-4111-8111-111111111111"
	if _, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false); err != nil {
		t.Fatal("parent rate did not refill")
	}
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	for _, extra := range []url.Values{{"ownerID": {"admin"}}, {"scope": {"all"}}, {"request_id": {"x", "x"}}, {"session_id": {"not-a-uuid"}}} {
		form := directForm()
		for k, v := range extra {
			form[k] = v
		}
		if w := directPOST(t, h, parent, form); w.Code != 400 {
			t.Fatal("invalid HTTP request accepted")
		}
	}
	form := directForm()
	form.Set("requested_nickname", strings.Repeat("x", 17<<10))
	if w := directPOST(t, h, parent, form); w.Code != 400 {
		t.Fatal("oversized direct body accepted")
	}
}
