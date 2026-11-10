package serverruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

func TestProtectedNewRegistrationRevokedAfterClaimDoesNotCommit(t *testing.T) {
	store := protectedStore(t)
	grant, _ := protectedGrant(t, store, "new-client")
	claim := grant.Claim
	grant.Claim = func(identity string) error {
		if err := claim(identity); err != nil {
			return err
		}
		return store.RevokeToken("project", grant.TokenID)
	}
	backend := newProtectedBackend("project", broker.NewRemoteService())
	request := broker.RegisterRequest{Holder: g4FixtureHolder(), Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: t.TempDir(), ClientID: "claim-race-client", Name: "ignored", ChannelID: "claim-race-lease", Secret: strings.Repeat("s", 40), RequestID: "claim-race-request"}
	result, err := backend.RegisterSession(access.WithGrant(t.Context(), grant), request)
	if err == nil && result.Code == "" {
		t.Fatal("registration committed after its token was revoked")
	}
	sessions, err := backend.ListSessions(t.Context())
	if err != nil || len(sessions) != 0 {
		t.Fatal("revoked first registration left a roster entry")
	}
}

func protectedRequest(n int, name string) broker.RegisterRequest {
	return broker.RegisterRequest{Holder: g4FixtureHolder(), Harness: "codex", SessionID: fmt.Sprintf("11111111-1111-4111-8111-%012d", n), PID: n + 100, CWD: "/work", ClientID: fmt.Sprintf("client-%04d", n), Name: name, ChannelID: fmt.Sprintf("lease-%d", n), Secret: "fixture-resume-secret-long-enough-for-registration", RequestID: fmt.Sprintf("request-%d", n)}
}
func protectedStore(t *testing.T) *controlpanel.Store {
	t.Helper()
	s, err := controlpanel.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Setup("fixture-password-long-enough"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProject("project", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	return s
}
func protectedGrant(t *testing.T, s *controlpanel.Store, nickname string) (access.Grant, string) {
	t.Helper()
	raw, err := s.IssueToken("project", nickname)
	if err != nil {
		t.Fatal(err)
	}
	var grant access.Grant
	h := controlpanel.New(s, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		grant, ok = access.FromContext(r.Context())
		if !ok {
			t.Error("missing grant")
		}
	}))
	req := httptest.NewRequest("GET", "https://broker/mcp?project=project", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal("grant request", w.Code)
	}
	return grant, raw
}
func protectedRegistered(t *testing.T, b *broker.RemoteService, r broker.RegisterRequest) broker.Session {
	t.Helper()
	out, err := b.RegisterSession(context.Background(), r)
	if err != nil || out.Code != "" {
		t.Fatal("registration failed", out.Code, err)
	}
	return out.Session
}

func TestProtectedOperationsDenyOtherAndUnboundIdentities(t *testing.T) {
	for _, unbound := range []bool{false, true} {
		t.Run(fmt.Sprint(unbound), func(t *testing.T) {
			store := protectedStore(t)
			raw := broker.NewRemoteService()
			defer raw.Close()
			a := protectedRequest(1, "alpha")
			b := protectedRequest(2, "beta")
			protectedRegistered(t, raw, a)
			target := protectedRegistered(t, raw, b)
			grantName := "alpha"
			if unbound {
				grantName = "beta"
			}
			g, _ := protectedGrant(t, store, grantName)
			if !unbound {
				if err := g.Claim(bindingIdentity(a.Harness, a.SessionID, a.ClientID)); err != nil {
					t.Fatal(err)
				}
			}
			service := newProtectedBackend("project", raw)
			ctx := access.WithGrant(context.Background(), g)
			operations := map[string]func() error{
				"send": func() error {
					_, e := service.SendMessage(ctx, broker.Message{From: "beta", To: "alpha", Body: "forged"})
					return e
				},
				"wait": func() error {
					c, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()
					_, e := service.WaitForIngress(c, "beta", b.ChannelID)
					return e
				},
				"failure": func() error { return service.ReportIngressFailure(ctx, target.RegistrationID, b.ChannelID) },

				"renew": func() error {
					o, e := service.RenewLease(ctx, broker.RenewRequest{RegistrationID: target.RegistrationID, ChannelID: b.ChannelID})
					if o.Code == broker.CodeInvalidToken {
						return errProtectedAccess
					}
					return e
				},
				"probe": func() error {
					o, e := service.AnswerProbe(ctx, broker.ProbeAnswer{RegistrationID: target.RegistrationID, ChannelID: b.ChannelID, ProbeID: "probe", Revision: 1})
					if o.Code == broker.CodeInvalidToken {
						return errProtectedAccess
					}
					return e
				},
				"resume": func() error {
					o, e := service.ResumeSession(ctx, broker.ResumeRequest{Holder: g4FixtureHolder(), RegistrationID: target.RegistrationID, Harness: b.Harness, SessionID: b.SessionID, ClientID: b.ClientID, PID: b.PID, CWD: b.CWD, ChannelID: "new-lease", Secret: b.Secret, RequestID: "resume", ExpectedRevision: target.Revision})
					if o.Code == broker.CodeInvalidToken {
						return errProtectedAccess
					}
					return e
				},
			}
			for name, operation := range operations {
				if err := operation(); !errors.Is(err, errProtectedAccess) {
					t.Errorf("%s did not deny: %v", name, err)
				}
			}
			if unbound && store.Projects()[0].Tokens[0].BoundIdentity != "" {
				t.Fatal("operation bound token")
			}
			state, err := raw.GetSession(context.Background(), "beta")
			if err != nil || state.Status != broker.StatusAlive || state.Revision != target.Revision {
				t.Fatal("denied operation mutated target", state, err)
			}
		})
	}
}

func TestProtectedAdmissionAssignsUUIDAndOpenFallback(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	service := newProtectedBackend("project", remote)
	nickname := "123e4567-e89b-12d3-a456-426614174000"
	g, _ := protectedGrant(t, store, nickname)
	ctx := access.WithGrant(context.Background(), g)
	r := protectedRequest(1, "client-spoof")
	out, err := service.RegisterSession(ctx, r)
	if err != nil || out.Code != "" || out.Session.Name != nickname {
		t.Fatal("authoritative UUID nickname lost", out, err)
	}
	if g.Check(bindingIdentity(r.Harness, r.SessionID, r.ClientID)) != nil {
		t.Fatal("admission core did not call Claim")
	}
	if _, err = service.AttachSession(ctx, broker.AttachRequest{Harness: r.Harness, SessionID: r.SessionID, PID: r.PID, CWD: r.CWD, Name: nickname}); !errors.Is(err, errProtectedAccess) {
		t.Fatal("legacy attach bypass", err)
	}
	first := protectedRegistered(t, remote, protectedRequest(2, "open-name"))
	second, err := service.RegisterSession(context.Background(), protectedRequest(3, "open-name"))
	if err != nil || second.Code != "" || second.Warning == "" || second.Session.Name == first.Name {
		t.Fatal("open fallback lost", second, err)
	}
}

func TestProtectedAdmissionWrongSecretDoesNotBind(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	r := protectedRequest(1, "alpha")
	existing := protectedRegistered(t, remote, r)
	g, _ := protectedGrant(t, store, "alpha")
	service := newProtectedBackend("project", remote)
	ctx := access.WithGrant(context.Background(), g)
	r.Secret = "different-resume-secret-long-enough-but-incorrect"
	r.ChannelID = "new-lease"
	r.RequestID = "attack"
	out, err := service.RegisterSession(ctx, r)
	if err != nil || out.Code != broker.CodeOwned {
		t.Fatal("wrong secret not rejected", out.Code, err)
	}
	if store.Projects()[0].Tokens[0].BoundIdentity != "" {
		t.Fatal("wrong secret bound token")
	}
	now, _ := remote.GetSession(context.Background(), existing.RegistrationID)
	if now.Revision != existing.Revision {
		t.Fatal("wrong secret changed record")
	}
}

func TestProtectedRevocationAndWaitCancellation(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	r := protectedRequest(1, "alpha")
	protectedRegistered(t, remote, r)
	g, token := protectedGrant(t, store, "alpha")
	if err := g.Claim(bindingIdentity(r.Harness, r.SessionID, r.ClientID)); err != nil {
		t.Fatal(err)
	}
	service := newProtectedBackend("project", remote)
	entered := make(chan struct{})
	done := make(chan error, 1)
	h := controlpanel.New(store, nil, http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		close(entered)
		_, err := service.WaitForIngress(rq.Context(), "alpha", r.ChannelID)
		done <- err
	}))
	req := httptest.NewRequest("GET", "https://broker/mcp?project=project", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	go h.ServeHTTP(httptest.NewRecorder(), req)
	<-entered
	if err := store.RevokeToken("project", g.TokenID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, errProtectedAccess) {
			t.Fatal("wait cancellation lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("revocation wait hung")
	}
	if err := service.StopSession(access.WithGrant(context.Background(), g), "alpha"); !errors.Is(err, errProtectedAccess) {
		t.Fatal("revoked grant permitted mutation", err)
	}
}

func TestProtectedValidIdentityCanExchangeAndResume(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	a, b := protectedRequest(1, "alpha"), protectedRequest(2, "beta")
	sa := protectedRegistered(t, remote, a)
	protectedRegistered(t, remote, b)
	ga, _ := protectedGrant(t, store, "alpha")
	gb, _ := protectedGrant(t, store, "beta")
	if err := ga.Claim(bindingIdentity(a.Harness, a.SessionID, a.ClientID)); err != nil {
		t.Fatal(err)
	}
	if err := gb.Claim(bindingIdentity(b.Harness, b.SessionID, b.ClientID)); err != nil {
		t.Fatal(err)
	}
	service := newProtectedBackend("project", remote)
	ca, cb := access.WithGrant(context.Background(), ga), access.WithGrant(context.Background(), gb)
	sent, err := service.SendMessage(ca, broker.Message{From: "alpha", To: "beta", Body: "hello"})
	if err != nil || !sent.Accepted {
		t.Fatal("owner send denied", sent, err)
	}
	wait, cancel := context.WithTimeout(cb, time.Second)
	defer cancel()
	message, err := service.WaitForIngress(wait, "beta", b.ChannelID)
	if err != nil || message.From != "alpha" || message.Body != "hello" {
		t.Fatal("owner receive failed", message, err)
	}
	renewed, err := service.RenewLease(ca, broker.RenewRequest{RegistrationID: sa.RegistrationID, ChannelID: a.ChannelID})
	if err != nil || renewed.Code != "" {
		t.Fatal("owner renew denied", renewed.Code, err)
	}
	resumed, err := service.ResumeSession(ca, broker.ResumeRequest{Holder: g4FixtureHolder(), RegistrationID: sa.RegistrationID, Harness: a.Harness, SessionID: a.SessionID, ClientID: a.ClientID, PID: a.PID, CWD: a.CWD, ChannelID: "resumed-lease", Secret: a.Secret, RequestID: "resume-owner", ExpectedRevision: sa.Revision})
	if err != nil || resumed.Code != "" {
		t.Fatal("owner resume denied", resumed.Code, err)
	}
	if err = service.StopSession(cb, "beta"); err != nil {
		t.Fatal("owner detach denied", err)
	}
}

func TestProtectedCoreRechecksBeforeMutation(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	a, b := protectedRequest(1, "alpha"), protectedRequest(2, "beta")
	protectedRegistered(t, remote, a)
	protectedRegistered(t, remote, b)
	g, _ := protectedGrant(t, store, "alpha")
	if err := g.Claim(bindingIdentity(a.Harness, a.SessionID, a.ClientID)); err != nil {
		t.Fatal(err)
	}
	// Revoke immediately after the outer lookup's successful check, before core
	// resolves and mutates the target. The core callback must see the revocation.
	check := g.Check
	g.Check = func(identity string) error {
		err := check(identity)
		if err == nil {
			if revokeErr := store.RevokeToken("project", g.TokenID); revokeErr != nil {
				t.Error(revokeErr)
			}
		}
		return err
	}
	service := newProtectedBackend("project", remote)
	out, err := service.SendMessage(access.WithGrant(context.Background(), g), broker.Message{From: "alpha", To: "beta", Body: "must not queue"})
	if err == nil || out.Accepted {
		t.Fatal("core mutation did not recheck revoked grant")
	}
	target, err := remote.GetSession(context.Background(), "beta")
	if err != nil || target.PendingMessages != 0 {
		t.Fatal("denied mutation queued message", target, err)
	}
}

func TestProtectedStaleAndCrossProjectGrantDenied(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	r := protectedRequest(1, "alpha")
	session := protectedRegistered(t, remote, r)
	g, _ := protectedGrant(t, store, "alpha")
	if err := g.Claim(bindingIdentity(r.Harness, r.SessionID, r.ClientID)); err != nil {
		t.Fatal(err)
	}
	other := newProtectedBackend("other-project", remote)
	if err := other.CheckSession(access.WithGrant(context.Background(), g), session); !errors.Is(err, errProtectedAccess) {
		t.Fatal("cross-project grant accepted", err)
	}
	service := newProtectedBackend("project", remote)
	if err := store.SaveProject("project", "token", false, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProject("project", "token", true, ""); err != nil {
		t.Fatal(err)
	}
	if err := service.StopSession(access.WithGrant(context.Background(), g), "alpha"); !errors.Is(err, errProtectedAccess) {
		t.Fatal("stale grant accepted", err)
	}
}

func TestProtectedRevokedAdmissionReplayDenied(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	service := newProtectedBackend("project", remote)
	g, _ := protectedGrant(t, store, "alpha")
	ctx := access.WithGrant(context.Background(), g)
	r := protectedRequest(1, "alpha")
	out, err := service.RegisterSession(ctx, r)
	if err != nil || out.Code != "" {
		t.Fatal("register failed", out, err)
	}
	if err := store.RevokeToken("project", g.TokenID); err != nil {
		t.Fatal(err)
	}
	replay, err := service.RegisterSession(ctx, r)
	if err == nil && replay.Code == "" {
		t.Fatal("revoked grant replay returned authorized session")
	}
}

func TestProtectedExplicitRegisterClaimsNewTokenForExistingIdentity(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	service := newProtectedBackend("project", remote)
	r := protectedRequest(1, "alpha")
	// An existing unbound record can come from a legacy project migration.
	// Seed the trusted backend directly; production no longer allows open mode.
	existing := protectedRegistered(t, remote, r)
	grant, _ := protectedGrant(t, store, "alpha")
	for attempt := 0; attempt < 2; attempt++ {
		ctx := access.WithGrant(context.Background(), grant)
		r.ExpectedRevision = existing.Revision
		r.RequestID = fmt.Sprintf("explicit-%d", attempt)
		r.ChannelID = fmt.Sprintf("explicit-lease-%d", attempt)
		wrong := r
		wrong.Secret = "incorrect-resume-secret-long-enough-for-validation"
		denied, err := service.RegisterSession(ctx, wrong)
		if err != nil || denied.Code != broker.CodeOwned {
			t.Fatal("wrong secret not rejected", denied.Code, err)
		}
		if grant.Check(bindingIdentity(r.Harness, r.SessionID, r.ClientID)) == nil {
			t.Fatal("wrong-secret registration bound token")
		}
		admitted, err := service.RegisterSession(ctx, r)
		if err != nil || admitted.Code != "" || admitted.Session.Name != "alpha" || admitted.Session.RegistrationID != existing.RegistrationID {
			t.Fatalf("explicit registration with token generation %d rejected: code=%s error=%v", attempt, admitted.Code, err)
		}
		if err := grant.Check(bindingIdentity(r.Harness, r.SessionID, r.ClientID)); err != nil {
			t.Fatal("successful explicit registration did not bind", err)
		}
		existing = admitted.Session
		if attempt == 0 {
			if err := store.RevokeToken("project", grant.TokenID); err != nil {
				t.Fatal(err)
			}
			grant, _ = protectedGrant(t, store, "alpha")
		}
	}
}
