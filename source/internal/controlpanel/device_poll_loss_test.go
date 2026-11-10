package controlpanel

import (
	"errors"
	"testing"
	"time"
)

func TestDevicePollLostSlowdownStillGrantsAtClientInterval(t *testing.T) {
	f := newDeviceFixture(t)
	code, req := polledDevice(t, f, 1)
	f.now = f.now.Add(4 * time.Second)
	if _, err := f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false); !errors.Is(err, errDeviceSlow) {
		t.Fatal("early poll not throttled")
	}
	// This response is lost; the client retains 5s and backs off 2x for transport failure.
	f.now = f.now.Add(10 * time.Second)
	if _, err := f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false); !errors.Is(err, errDevicePending) {
		t.Fatal("pending poll failed")
	}
	if _, err := f.store.ApproveDeviceAuthorization("owner", code.UserCode, "poll-agent", false); err != nil {
		t.Fatal("approval failed")
	}
	interval := 5 * time.Second
	for i := 0; i < 4; i++ {
		f.now = f.now.Add(interval)
		token, err := f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false)
		if err == nil && token.AccessToken != "" {
			return
		}
		if !errors.Is(err, errDeviceSlow) {
			t.Fatal("unexpected poll outcome")
		}
		interval += 5 * time.Second
	}
	t.Fatal("client/server poll intervals diverged after a lost slowdown")
}
