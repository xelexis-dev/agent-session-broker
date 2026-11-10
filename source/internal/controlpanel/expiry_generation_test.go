package controlpanel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func churnStore(t *testing.T) (*Store, func(time.Duration)) {
	t.Helper()
	s := directStore(t)
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	return s, func(d time.Duration) { clock.Add(int64(d)) }
}

type churnCredential struct {
	raw      string
	identity string
	grant    access.Grant
	changes  <-chan struct{}
}

func churnCredentialFor(t *testing.T, s *Store, parent string, index int, confirmed bool) churnCredential {
	t.Helper()
	r := directRequest(directForm())
	r.SessionID = fmt.Sprintf("%08d-1111-4111-8111-111111111111", index)
	r.RequestID = fmt.Sprintf("churn-request-%d", index)
	r.RequestedNickname = fmt.Sprintf("churn-agent-%d", index)
	issued, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "fixture", false)
	if err != nil {
		t.Fatal("exchange fixture failed", err)
	}
	g, changes, ok := s.authorize("demo", issued.AccessToken)
	if !ok {
		t.Fatal("credential fixture unauthorized")
	}
	if confirmed {
		if err := g.Claim(r.identity()); err != nil {
			t.Fatal("claim fixture failed", err)
		}
		registration := fmt.Sprintf("churn-registration-%d", index)
		if err := g.Registration(r.identity(), registration, "", g.Nickname, 0, false); err != nil {
			t.Fatal("claiming fixture failed", err)
		}
		if err := g.Registration(r.identity(), registration, g.OwnerID, g.Nickname, 1, true); err != nil {
			t.Fatal("confirmation fixture failed", err)
		}
	}
	return churnCredential{issued.AccessToken, r.identity(), g, changes}
}

func assertChurnGrantHealthy(t *testing.T, c churnCredential) {
	t.Helper()
	if err := c.grant.Check(c.identity); err != nil {
		t.Errorf("existing confirmed grant invalidated: %v", err)
	}
	select {
	case <-c.changes:
		t.Error("unrelated generation closed")
	default:
	}
	select {
	case <-c.grant.Revoked:
		t.Error("unrelated token revoked")
	default:
	}
}

func TestExpiryGenerationAutoExchangePrunePreservesConfirmedGrant(t *testing.T) {
	s, advance := churnStore(t)
	parent := directParent(t, s, 8)
	a := churnCredentialFor(t, s, parent, 1, true)
	_ = churnCredentialFor(t, s, parent, 2, true)
	assertChurnGrantHealthy(t, a)
	if len(s.data.Projects["demo"].AutoExchanges) != 2 {
		t.Fatal("exchange fixture missing")
	}
	advance(directWindow + time.Second)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	if len(s.data.Projects["demo"].AutoExchanges) != 0 {
		t.Error("expired exchanges retained")
	}
	assertChurnGrantHealthy(t, a)
}

func TestExpiryGenerationIssuedReservationRevokesOnlyItsToken(t *testing.T) {
	s, advance := churnStore(t)
	parent := directParent(t, s, 8)
	a := churnCredentialFor(t, s, parent, 1, true)
	b := churnCredentialFor(t, s, parent, 2, false)
	advance(directWindow + time.Second)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.grant.Revoked:
	default:
		t.Error("issued reservation did not revoke its token")
	}
	if _, _, ok := s.authorize("demo", b.raw); ok {
		t.Error("expired reservation authorized")
	}
	assertChurnGrantHealthy(t, a)
}

func TestExpiryGenerationAbsoluteSessionExpiryIsIsolated(t *testing.T) {
	s, advance := churnStore(t)
	parent := directParent(t, s, 8)
	a := churnCredentialFor(t, s, parent, 1, true)
	b := churnCredentialFor(t, s, parent, 2, true)
	s.mu.Lock()
	p := s.data.Projects["demo"]
	p.Tokens = append([]SessionToken(nil), p.Tokens...)
	for i := range p.Tokens {
		if p.Tokens[i].ID == a.grant.TokenID {
			p.Tokens[i].ExpiresAt = s.now().Add(time.Minute)
		}
	}
	err := s.commitProject(p)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	advance(2 * time.Minute)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.grant.Revoked:
	default:
		t.Error("absolute expiry did not revoke its token")
	}
	if !errors.Is(a.grant.Check(a.identity), ErrInvalid) {
		t.Error("expired session Check succeeded")
	}
	assertChurnGrantHealthy(t, b)
}

func TestExpiryGenerationHTTPStaysOpenOnOtherExchangeExpiry(t *testing.T) {
	s, advance := churnStore(t)
	parent := directParent(t, s, 8)
	a := churnCredentialFor(t, s, parent, 1, true)
	// Remove A's exchange first; the open request below observes only B's expiry.
	advance(directWindow + time.Second)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	_ = churnCredentialFor(t, s, parent, 2, true)
	entered, done := make(chan struct{}), make(chan struct{})
	h := New(s, &panelBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "https://broker/mcp?project=demo", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+a.raw)
	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), req) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("fixture request did not finish")
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture request not entered")
	}
	advance(directWindow + time.Second)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Error("other exchange expiry cancelled open HTTP request")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestExpiryGenerationFailedReservationSweepPublishesNoSignals(t *testing.T) {
	s, advance := churnStore(t)
	parent := directParent(t, s, 8)
	a := churnCredentialFor(t, s, parent, 1, true)
	b := churnCredentialFor(t, s, parent, 2, false)
	write, batch := s.write, s.writeBatch
	s.write = func([]byte) error { return ErrStorage }
	s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	advance(directWindow + time.Second)
	if err := s.SweepExpired(); err == nil {
		t.Fatal("failed sweep succeeded")
	}
	select {
	case <-a.changes:
		t.Error("failed write cancelled generation")
	default:
	}
	select {
	case <-b.grant.Revoked:
		t.Error("failed write published revocation")
	default:
	}
	if len(s.data.Projects["demo"].AutoExchanges) != 2 {
		t.Error("failed write published exchange pruning")
	}
	for _, token := range s.data.Projects["demo"].Tokens {
		if token.ID == b.grant.TokenID && token.Revoked {
			t.Error("failed write published token revocation")
		}
	}
	s.write, s.writeBatch = write, batch
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.grant.Revoked:
	default:
		t.Error("successful retry did not revoke issued reservation")
	}
	assertChurnGrantHealthy(t, a)
}
