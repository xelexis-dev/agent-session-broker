package serverruntime

import (
	"errors"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestProtectedConnectionConfirmedOnlyAfterSuccessfulRegistration(t *testing.T) {
	store := protectedStore(t)
	grant, _ := protectedGrant(t, store, "connected-client")
	raw := broker.NewRemoteService()
	t.Cleanup(func() { raw.Close() })
	backend := newProtectedBackend("project", raw)
	request := protectedRequest(401, "ignored")
	confirmed := 0
	connected := grant.Connected
	grant.Connected = func(identity string) error {
		confirmed++
		session, err := raw.GetSession(t.Context(), "name:connected-client")
		if err != nil || session.RegistrationID == "" {
			t.Fatal("connection confirmed before durable registration exists", err)
		}
		if identity != bindingIdentity(request.Harness, request.SessionID, request.ClientID) {
			t.Fatal("wrong confirmed identity")
		}
		return connected(identity)
	}
	ctx := access.WithGrant(t.Context(), grant)
	invalid := request
	invalid.Secret = "short"
	if result, err := backend.RegisterSession(ctx, invalid); err == nil && result.Code == "" {
		t.Fatal("invalid registration accepted")
	}
	if confirmed != 0 {
		t.Fatal("failed registration confirmed")
	}
	result, err := backend.RegisterSession(ctx, request)
	if err != nil || result.Code != "" || confirmed != 1 {
		t.Fatal("successful registration was not confirmed", result.Code, err, confirmed)
	}
}

func TestProtectedConnectionConfirmationFailureCanRetry(t *testing.T) {
	store := protectedStore(t)
	grant, _ := protectedGrant(t, store, "retry-client")
	raw := broker.NewRemoteService()
	t.Cleanup(func() { raw.Close() })
	backend := newProtectedBackend("project", raw)
	request := protectedRequest(402, "ignored")
	connected := grant.Connected
	grant.Connected = func(string) error { return errors.New("confirmation storage unavailable") }
	result, err := backend.RegisterSession(access.WithGrant(t.Context(), grant), request)
	if err == nil && result.Code == "" {
		t.Fatal("unconfirmed registration acknowledged")
	}
	grant.Connected = connected
	result, err = backend.RegisterSession(access.WithGrant(t.Context(), grant), request)
	if err != nil || result.Code != "" {
		t.Fatal("confirmation replay failed", result.Code, err)
	}
}
