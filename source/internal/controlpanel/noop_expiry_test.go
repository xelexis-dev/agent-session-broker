package controlpanel

import (
	"testing"
	"time"
)

func TestNoopExpiryAndGrantCheckAllocateNothing(t *testing.T) {
	s, _ := churnStore(t)
	parent := directParent(t, s, 8)
	child := churnCredentialFor(t, s, parent, 801, true)
	_, err := s.BeginDeviceAuthorization("demo", parent, DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", InstanceID: "noop-device-client", SessionID: "00000000-1111-4111-8111-111111111111", RequestID: "noop-device-request"})
	if err != nil {
		t.Fatal(err)
	}
	var sweepErr error
	allocations := testing.AllocsPerRun(100, func() { s.mu.Lock(); sweepErr = s.expireLocked(); s.mu.Unlock() })
	if sweepErr != nil {
		t.Fatal(sweepErr)
	}
	if allocations != 0 {
		t.Errorf("no-op expiry allocated %.0f objects per call, want 0", allocations)
	}
	var checkErr error
	allocations = testing.AllocsPerRun(100, func() { checkErr = child.grant.Check(child.identity) })
	if checkErr != nil {
		t.Fatal(checkErr)
	}
	if allocations != 0 {
		t.Errorf("no-op grant check allocated %.0f objects per call, want 0", allocations)
	}
	assertChurnGrantHealthy(t, child)
	// Retained revoked history is also a no-op after its transition was published.
	retired := churnCredentialFor(t, s, parent, 804, true)
	if err := s.RevokeToken("demo", retired.grant.TokenID); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	fresh, changes, ok := s.authorize("demo", child.raw)
	if !ok {
		t.Fatal("refresh after explicit revoke")
	}
	allocations = testing.AllocsPerRun(100, func() { s.mu.Lock(); sweepErr = s.expireLocked(); s.mu.Unlock() })
	if sweepErr != nil || allocations != 0 {
		t.Fatalf("retained history caused no-op expiry allocations: %.0f", allocations)
	}
	if fresh.Check(child.identity) != nil {
		t.Fatal("no-op sweep invalidated current grant")
	}
	select {
	case <-changes:
		t.Fatal("no-op sweep closed current generation")
	default:
	}
}

func TestNoopExpiryTransitionsAtReservationBoundary(t *testing.T) {
	s, advance := churnStore(t)
	parent := directParent(t, s, 8)
	confirmed := churnCredentialFor(t, s, parent, 802, true)
	issued := churnCredentialFor(t, s, parent, 803, false)
	advance(directWindow - time.Nanosecond)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-issued.grant.Revoked:
		t.Fatal("reservation revoked before boundary")
	default:
	}
	assertChurnGrantHealthy(t, confirmed)
	advance(time.Nanosecond)
	if err := s.SweepExpired(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-issued.grant.Revoked:
	default:
		t.Fatal("reservation watch remained open at exact expiry")
	}
	if issued.grant.Check(issued.identity) == nil {
		t.Fatal("expired reservation remains usable")
	}
	assertChurnGrantHealthy(t, confirmed)
}
