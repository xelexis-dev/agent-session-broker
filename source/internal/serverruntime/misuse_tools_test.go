package serverruntime

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"testing"
)

func TestMisuseA4ManagementSelectorsAndRepeatedActions(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	backend := newProtectedBackend("project", remote)
	grant, caller := manageRegistered(t, store, backend, 981, "admin", "caller")
	targetGrant, target := manageRegistered(t, store, backend, 982, "admin", "target")
	ctx := access.WithGrant(t.Context(), grant)
	foreign := broker.NewRemoteService()
	defer foreign.Close()
	other := protectedRegistered(t, foreign, protectedRequest(983, "foreign"))
	for _, name := range []string{"missing", other.ID, other.RegistrationID, "registration:" + other.RegistrationID, "session:" + other.ID} {
		for _, action := range []string{"detached", "suspended", "unsuspended"} {
			if err := manageOperation(backend, ctx, action, name); err == nil {
				t.Fatal("unknown or cross-project management succeeded")
			}
		}
	}
	for _, action := range []string{"suspended", "unsuspended"} {
		if err := manageOperation(backend, ctx, action, caller.Name); err == nil {
			t.Fatal("self suspend or unsuspend succeeded")
		}
	}
	if err := backend.UnsuspendSession(ctx, target.Name); err != nil {
		t.Fatal("idempotent unsuspend failed")
	}
	for i := 0; i < 2; i++ {
		if err := backend.SuspendSession(ctx, target.Name); err != nil {
			t.Fatal("repeated suspend failed")
		}
	}
	suspended, _ := remote.GetSession(t.Context(), target.Name)
	if suspended.Status != "suspended" || suspended.AutomaticDeliveryReady {
		t.Fatal("suspend state invalid")
	}
	targetCtx := access.WithGrant(t.Context(), targetGrant)
	if _, err := backend.SendMessage(targetCtx, broker.Message{From: target.Name, To: caller.Name, Body: "blocked"}); err == nil {
		t.Fatal("suspended sender delivered")
	}
	if _, err := backend.SendMessage(ctx, broker.Message{From: caller.Name, To: target.Name, Body: "blocked"}); err == nil {
		t.Fatal("message delivered to suspended target")
	}
	req := protectedRequest(982, "target")
	if got, err := backend.RenewLease(targetCtx, broker.RenewRequest{RegistrationID: target.RegistrationID, ChannelID: req.ChannelID}); err == nil && got.Code == "" {
		t.Fatal("suspended credential renewed")
	}
	for i := 0; i < 2; i++ {
		if err := backend.UnsuspendSession(ctx, target.Name); err != nil {
			t.Fatal("repeated unsuspend failed")
		}
	}
	for i := 0; i < 2; i++ {
		if err := backend.StopSession(ctx, target.Name); err != nil {
			t.Fatal("repeated stop failed")
		}
	}
	detached, _ := remote.GetSession(t.Context(), target.Name)
	if detached.ErrorCode != broker.CodeDetached || detached.AutomaticDeliveryReady {
		t.Fatal("detach state invalid")
	}
	if _, err := backend.SendMessage(ctx, broker.Message{From: caller.Name, To: target.Name, Body: "blocked"}); err == nil {
		t.Fatal("message delivered to detached target")
	}
	if err := backend.StopSession(ctx, caller.Name); err != nil {
		t.Fatal("self detach denied")
	}
}
