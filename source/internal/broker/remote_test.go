package broker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func remoteIdentity(n int) AttachRequest {
	return AttachRequest{Name: fmt.Sprintf("pc-%d", n), Harness: "codex", SessionID: fmt.Sprintf("00000000-0000-0000-0000-%012d", n), PID: 1234, CWD: `C:\foreign\project`, ChannelID: fmt.Sprintf("lease-%d", n)}
}

func TestRemoteIndependentHostsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewRemoteService()
	a, b := remoteIdentity(1), remoteIdentity(2)
	b.Harness = "claude"
	for _, id := range []AttachRequest{a, b} {
		got, e := s.AttachSession(ctx, id)
		if e != nil || got.PID != 1234 || !got.AutomaticDeliveryReady || got.Status != StatusAlive {
			t.Fatalf("attach: %+v %v", got, e)
		}
	}
	for _, pair := range [][2]AttachRequest{{a, b}, {b, a}} {
		sent, e := s.SendMessage(ctx, Message{From: pair[0].Name, To: pair[1].Name, Body: "PID=1234"})
		if e != nil || !sent.Accepted {
			t.Fatal(sent, e)
		}
		got, e := s.WaitForIngress(ctx, pair[1].Name, pair[1].ChannelID)
		if e != nil || got.From != pair[0].Name || got.Body != "PID=1234" {
			t.Fatal(got, e)
		}
	}
	if _, e := s.WaitForIngress(ctx, a.Name, "wrong"); e == nil {
		t.Fatal("foreign receiver accepted")
	}
	if _, e := NewRemoteService().GetSession(ctx, a.Name); e == nil {
		t.Fatal("project leaked")
	}
}

func TestRemoteCollisionExpiryAndGeneration(t *testing.T) {
	ctx := context.Background()
	s := NewRemoteService()
	now := time.Now()
	s.now = func() time.Time { return now }
	a := remoteIdentity(1)
	first, e := s.AttachSession(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	collision := remoteIdentity(2)
	collision.Name = a.Name
	if _, e = s.AttachSession(ctx, collision); e == nil {
		t.Fatal("name stolen")
	}
	renamed := a
	renamed.Name = "renamed"
	if _, e = s.AttachSession(ctx, renamed); e == nil {
		t.Fatal("identity registered twice")
	}
	now = now.Add(31 * time.Second)
	got, e := s.GetSession(ctx, a.Name)
	if e != nil || got.Status != StatusMissing || got.AutomaticDeliveryReady {
		t.Fatal(got, e)
	}
	if _, e = s.SendMessage(ctx, Message{From: a.Name, To: a.Name, Body: "expired"}); e == nil {
		t.Fatal("expired send accepted")
	}
	if _, e = s.AttachSession(ctx, a); e == nil {
		t.Fatal("expired receiver resurrected")
	}
	second, e := s.AttachSession(ctx, collision)
	if e != nil || first.Generation == second.Generation {
		t.Fatal(second, e)
	}
	if _, e = s.WaitForIngress(ctx, a.Name, a.ChannelID); e == nil {
		t.Fatal("old receiver stole replacement")
	}
	if e = s.ReportIngressFailure(ctx, a.SessionID, a.ChannelID); e == nil {
		t.Fatal("old failure changed replacement")
	}
}

func TestRemoteRetirementUnblocksWaitAndFailureIsVisible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s := NewRemoteService()
	a := remoteIdentity(1)
	if _, e := s.AttachSession(ctx, a); e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	go func() { _, e := s.WaitForIngress(ctx, a.Name, a.ChannelID); result <- e }()
	if e := s.ReportIngressFailure(ctx, a.SessionID, a.ChannelID); e != nil {
		t.Fatal(e)
	}
	if e := <-result; e == nil || errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("wait not retired", e)
	}
	got, _ := s.GetSession(ctx, a.Name)
	if got.Status != StatusDegraded || got.ErrorCode != "ingress-failed" {
		t.Fatal(got)
	}
	if _, e := s.AttachSession(ctx, a); e == nil {
		t.Fatal("failed lease revived")
	}
	if e := s.StopSession(ctx, a.Name); e != nil {
		t.Fatal(e)
	}
	if _, e := s.AttachSession(ctx, a); e == nil {
		t.Fatal("detached receiver revived")
	}
	a.ChannelID = "fresh-lease"
	if _, e := s.AttachSession(ctx, a); e != nil {
		t.Fatal(e)
	}
}

func TestRemoteQueueBoundsFIFOAndCancellation(t *testing.T) {
	ctx := context.Background()
	s := NewRemoteService()
	a := remoteIdentity(1)
	s.AttachSession(ctx, a)
	if _, e := s.SendMessage(ctx, Message{From: a.Name, To: a.Name, Body: string(make([]byte, 12001))}); e == nil {
		t.Fatal("oversize accepted")
	}
	for i := 0; i < 32; i++ {
		if _, e := s.SendMessage(ctx, Message{From: a.Name, To: a.Name, Body: fmt.Sprint(i)}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.SendMessage(ctx, Message{From: a.Name, To: a.Name, Body: "overflow"}); !errors.Is(e, ErrQueueFull) {
		t.Fatal(e)
	}
	for i := 0; i < 32; i++ {
		m, e := s.WaitForIngress(ctx, a.Name, a.ChannelID)
		if e != nil || m.Body != fmt.Sprint(i) {
			t.Fatal(m, e)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, e := s.WaitForIngress(canceled, a.Name, a.ChannelID); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestRemoteRetiredIdentityAndUUIDNameCannotAliasRouting(t *testing.T) {
	ctx := context.Background()
	s := NewRemoteService()
	a := remoteIdentity(1)
	if _, err := s.AttachSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.StopSession(ctx, a.Name); err != nil {
		t.Fatal(err)
	}
	renamed := a
	renamed.Name = "another-name"
	renamed.ChannelID = "new-lease"
	if _, err := s.AttachSession(ctx, renamed); err != nil {
		t.Fatalf("detached UUID could not register a new name: %v", err)
	}
	if _, err := s.GetSession(ctx, a.Name); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("previous name still routes to the renamed session: %v", err)
	}
	got, err := s.GetSession(ctx, a.SessionID)
	if err != nil || got.Name != renamed.Name {
		t.Fatalf("UUID did not resolve to its current registration: %+v %v", got, err)
	}
	listed, err := s.ListSessions(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("old name retained an alias: %+v %v", listed, err)
	}
	if err := s.ReportIngressFailure(ctx, a.SessionID, a.ChannelID); err == nil {
		t.Fatal("old failure changed the new registration")
	}
	if _, err := s.WaitForIngress(ctx, renamed.Name, a.ChannelID); err == nil {
		t.Fatal("old receiver accepted by new registration")
	}
	if err := s.StopSession(ctx, renamed.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachSession(ctx, a); err == nil {
		t.Fatal("old heartbeat resurrected an earlier name after a second detach")
	}
	replayed := renamed
	replayed.ChannelID = a.ChannelID
	if _, err := s.AttachSession(ctx, replayed); err == nil {
		t.Fatal("retired lease reused under the new name")
	}
	other := remoteIdentity(2)
	other.Name = a.SessionID
	if _, err := s.AttachSession(ctx, other); err == nil {
		t.Fatal("name shadows a UUID routing address")
	}
}

func TestRemoteRenameCollisionPreservesDetachedIdentity(t *testing.T) {
	ctx := context.Background()
	s := NewRemoteService()
	a, other := remoteIdentity(1), remoteIdentity(2)
	for _, id := range []AttachRequest{a, other} {
		if _, err := s.AttachSession(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.StopSession(ctx, a.Name); err != nil {
		t.Fatal(err)
	}
	r := a
	r.Name, r.ChannelID = other.Name, "replacement-lease"
	if _, err := s.AttachSession(ctx, r); err == nil {
		t.Fatal("detached session stole a live name")
	}
	old, err := s.GetSession(ctx, a.SessionID)
	if err != nil || old.Name != a.Name || old.ErrorCode != "detached" {
		t.Fatalf("failed rename lost the prior registration: %+v %v", old, err)
	}
	r.Name = "available"
	if _, err := s.AttachSession(ctx, r); err != nil {
		t.Fatalf("valid retry after name collision: %v", err)
	}
	if _, err := s.SendMessage(ctx, Message{From: other.Name, To: r.Name, Body: "new route"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.WaitForIngress(ctx, r.Name, r.ChannelID)
	if err != nil || got.Body != "new route" {
		t.Fatal(got, err)
	}
}

func TestRemoteRenameReusesItsSlotAtSessionCapacity(t *testing.T) {
	ctx := context.Background()
	s := NewRemoteService()
	for i := 1; i <= remoteSessionCapacity; i++ {
		if _, err := s.AttachSession(ctx, remoteIdentity(i)); err != nil {
			t.Fatal(err)
		}
	}
	r := remoteIdentity(1)
	if err := s.StopSession(ctx, r.Name); err != nil {
		t.Fatal(err)
	}
	r.Name, r.ChannelID = "renamed", "fresh-lease"
	if _, err := s.AttachSession(ctx, r); err != nil {
		t.Fatalf("rename required an extra session slot: %v", err)
	}
	if _, err := s.AttachSession(ctx, remoteIdentity(remoteSessionCapacity+1)); err == nil {
		t.Fatal("new identity bypassed the session capacity")
	}
}

func TestRemoteLeaseHistoryLimitPreservesRegistration(t *testing.T) {
	ctx := context.Background()
	s := NewRemoteService()
	r := remoteIdentity(1)
	for i := 0; i < remoteLeaseHistoryCapacity; i++ {
		r.ChannelID = fmt.Sprintf("lease-%d", i)
		if _, err := s.AttachSession(ctx, r); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AttachSession(ctx, r); err != nil {
			t.Fatalf("heartbeat consumed a lease history slot: %v", err)
		}
		if err := s.StopSession(ctx, r.Name); err != nil {
			t.Fatal(err)
		}
	}
	r.Name, r.ChannelID = "over-limit", "extra-lease"
	if _, err := s.AttachSession(ctx, r); err == nil {
		t.Fatal("unbounded ingress lease history")
	}
	got, err := s.GetSession(ctx, r.SessionID)
	if err != nil || got.Name != "pc-1" || got.ErrorCode != "detached" {
		t.Fatalf("failed registration discarded the previous identity: %+v %v", got, err)
	}
}
