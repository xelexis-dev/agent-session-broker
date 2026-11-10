package serverruntime

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"testing"
	"time"
)

func TestProtectedRenameRevalidatesOldGrantAndKeepsWaitingReceiver(t *testing.T) {
	store := protectedStore(t)
	grant, _ := protectedGrant(t, store, "before")
	service := newProtectedBackend("project", broker.NewRemoteService())
	defer service.Close()
	request := protectedRequest(1, "ignored")
	ctx := access.WithGrant(t.Context(), grant)
	registered, err := service.RegisterSession(ctx, request)
	if err != nil || registered.Code != "" {
		t.Fatal(registered, err)
	}
	if _, err := store.IssueToken("project", "reserved"); err != nil {
		t.Fatal(err)
	}
	rename := broker.RenameRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: request.ChannelID, Name: "reserved"}
	if _, err = service.RenameSession(ctx, rename); err == nil {
		t.Fatal("unconnected reserved name was stolen")
	}
	receiveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		m, err := service.WaitForIngress(receiveCtx, broker.SelectorRegistration+registered.Session.RegistrationID, request.ChannelID)
		if err == nil && m.Body != "kept" {
			t.Error(m)
		}
		done <- err
	}()
	rename.Name = "after"
	changed, err := service.RenameSession(ctx, rename)
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the pre-rename context, as a streaming receiver and overlapping
	// HTTP request do. Its snapshot nickname must not invalidate the owner.
	if _, err = service.SendMessage(ctx, broker.Message{From: "after", To: "after", Body: "kept"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = service.GetSession(ctx, "before"); err == nil {
		t.Fatal("old alias still routes")
	}
	if _, err = store.IssueToken("project", "before"); err != nil {
		t.Fatal("old reservation was not released", err)
	}
	request.ExpectedRevision, request.RequestID = changed.Revision, "reattach-with-stale-grant"
	refreshed, err := service.RegisterSession(ctx, request)
	if err != nil || refreshed.Code != "" || refreshed.Session.Name != "after" {
		t.Fatal("old grant reverted name", refreshed, err)
	}
	if err = store.RevokeToken("project", grant.TokenID); err != nil {
		t.Fatal(err)
	}
	rename.Name = "revoked"
	if _, err = service.RenameSession(ctx, rename); err == nil {
		t.Fatal("revoked credential renamed session")
	}
}

func TestProtectedConcurrentRenameOnlyOneWins(t *testing.T) {
	store := protectedStore(t)
	service := newProtectedBackend("project", broker.NewRemoteService())
	defer service.Close()
	type candidate struct {
		ctx     context.Context
		request broker.RenameRequest
	}
	var candidates []candidate
	for i, name := range []string{"one", "two"} {
		grant, _ := protectedGrant(t, store, name)
		ctx := access.WithGrant(t.Context(), grant)
		req := protectedRequest(i+1, "ignored")
		r, err := service.RegisterSession(ctx, req)
		if err != nil || r.Code != "" {
			t.Fatal(r, err)
		}
		candidates = append(candidates, candidate{ctx, broker.RenameRequest{Name: "winner", RegistrationID: r.Session.RegistrationID, ChannelID: req.ChannelID}})
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, c := range candidates {
		go func() { <-start; _, err := service.RenameSession(c.ctx, c.request); results <- err }()
	}
	close(start)
	successes := 0
	for range candidates {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent owners = %d", successes)
	}
}
