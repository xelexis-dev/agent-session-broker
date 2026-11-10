package controlpanel

import (
	"errors"
	"testing"
)

// A client that lost the answer to its code request retries with the same
// request id: the still-pending code is replaced, an approved one is not.
func TestBeginDeviceAuthorizationReplacesOnlyPendingCodeForRepeatedRequest(t *testing.T) {
	f := newDeviceFixture(t)
	req := DeviceRequest{ClientID: "asb-client-v1", Harness: "claude", SessionID: "11111111-1111-4111-8111-111111111111", InstanceID: "pc-example", RequestID: "request-lost"}
	first, err := f.store.BeginDeviceAuthorization("demo", f.raw, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.store.BeginDeviceAuthorization("demo", f.raw, req)
	if err != nil {
		t.Fatalf("retry with the same request id: %v", err)
	}
	if first.DeviceCode == second.DeviceCode || first.UserCode == second.UserCode {
		t.Fatal("retry reused the old codes")
	}
	f.store.mu.Lock()
	pending := len(f.store.data.Projects["demo"].Devices)
	f.store.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending codes after retry: %d", pending)
	}
	f.resetPoll()
	if _, err := f.store.ExchangeDeviceToken("demo", f.raw, first.DeviceCode, req.ClientID, false); !errors.Is(err, errDeviceGrant) {
		t.Fatalf("replaced code still exchanges: %v", err)
	}
	f.resetPoll()
	if _, err := f.store.ExchangeDeviceToken("demo", f.raw, second.DeviceCode, req.ClientID, false); !errors.Is(err, errDevicePending) {
		t.Fatalf("current code: %v", err)
	}
	if _, err := f.store.ApproveDeviceAuthorization("owner", second.UserCode, "agent-a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.BeginDeviceAuthorization("demo", f.raw, req); !errors.Is(err, errDeviceGrant) {
		t.Fatalf("retry cancelled an approved request: %v", err)
	}
	f.resetPoll()
	if _, err := f.store.ExchangeDeviceToken("demo", f.raw, second.DeviceCode, req.ClientID, false); err != nil {
		t.Fatalf("approved code lost after a late retry: %v", err)
	}
}
