package controlpanel

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func polledDevice(t *testing.T, f *deviceFixture, n int) (DeviceResponse, DeviceRequest) {
	t.Helper()
	req := DeviceRequest{ClientID: "asb-client-v1", Harness: "codex", SessionID: fmt.Sprintf("11111111-1111-4111-8111-%012d", n), InstanceID: "pc-example", RequestID: fmt.Sprint("poll-", n)}
	code, err := f.store.BeginDeviceAuthorization(f.project, f.raw, req)
	if err != nil {
		t.Fatal("begin device")
	}
	_, err = f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false)
	if !errors.Is(err, errDevicePending) {
		t.Fatal("pending poll")
	}
	return code, req
}
func pollKeys(t *testing.T, s *Store, want int) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	live := map[string]bool{}
	for _, project := range s.data.Projects {
		for _, device := range project.Devices {
			live[device.CodeHash] = true
		}
	}
	for hash := range s.devicePoll {
		if !live[hash] {
			t.Error("poll state for removed device")
		}
	}
	if len(s.devicePoll) != want {
		t.Fatalf("poll keys=%d, want %d", len(s.devicePoll), want)
	}
}

func TestDevicePollRepeatedExpirySweep(t *testing.T) {
	f := newDeviceFixture(t)
	for i := 0; i < 8; i++ {
		polledDevice(t, f, i)
		f.now = f.now.Add(deviceLifetime + time.Second)
		if f.store.SweepExpired() != nil {
			t.Fatal("sweep")
		}
		pollKeys(t, f.store, 0)
	}
	f.store.mu.RLock()
	devices := len(f.store.data.Projects[f.project].Devices)
	f.store.mu.RUnlock()
	if devices != 0 {
		t.Fatal("expired grants remain")
	}
	pollKeys(t, f.store, 0)
}
func TestDevicePollRemovalPaths(t *testing.T) {
	for _, kind := range []string{"replacement", "begin-expiry", "denied-expiry", "exchanged-expiry", "consumed-expiry", "revoke-expiry", "project-delete"} {
		t.Run(kind, func(t *testing.T) {
			f := newDeviceFixture(t)
			code, req := polledDevice(t, f, 1)
			switch kind {
			case "replacement":
				if _, err := f.store.BeginDeviceAuthorization(f.project, f.raw, req); err != nil {
					t.Fatal("replace")
				}
			case "begin-expiry":
				f.now = f.now.Add(deviceLifetime + time.Second)
				if _, err := f.store.BeginDeviceAuthorization(f.project, f.raw, DeviceRequest{ClientID: req.ClientID, Harness: req.Harness, SessionID: req.SessionID, InstanceID: req.InstanceID, RequestID: "another"}); err != nil {
					t.Fatal("begin after expiry")
				}
			case "denied-expiry":
				if _, err := f.store.ApproveDeviceAuthorization("owner", code.UserCode, "", true); err != nil {
					t.Fatal("deny")
				}
			case "exchanged-expiry", "consumed-expiry":
				if _, err := f.store.ApproveDeviceAuthorization("owner", code.UserCode, "poll-agent", false); err != nil {
					t.Fatal("approve")
				}
				f.now = f.now.Add(5 * time.Second)
				token, err := f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false)
				if err != nil {
					t.Fatal("exchange")
				}
				if kind == "consumed-expiry" && !f.store.Authorize(f.project, token.AccessToken) {
					t.Fatal("consume")
				}
			case "revoke-expiry":
				f.store.mu.RLock()
				parent := f.store.data.Projects[f.project].Devices[0].TokenID
				f.store.mu.RUnlock()
				if f.store.RevokeTokenFor("owner", f.project, parent) != nil {
					t.Fatal("revoke")
				}
			case "project-delete":
				if _, err := f.store.beginProjectDeletion("owner", f.project, f.project); err != nil {
					t.Fatal("delete project")
				}
			}
			if kind != "replacement" && kind != "begin-expiry" && kind != "project-delete" {
				pollKeys(t, f.store, 1)
				if kind == "exchanged-expiry" {
					if _, err := f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false); !errors.Is(err, errDeviceSlow) {
						t.Fatal("approved replay throttle changed")
					}
				}

				f.now = f.now.Add(deviceLifetime + time.Second)
				if f.store.SweepExpired() != nil {
					t.Fatal("sweep")
				}
			}
			pollKeys(t, f.store, 0)
		})
	}
}
func TestDevicePollLiveSlowdownAndConcurrentSweep(t *testing.T) {
	f := newDeviceFixture(t)
	code, req := polledDevice(t, f, 1)
	for i := 0; i < 2; i++ {
		f.now = f.now.Add(time.Second)
		_, err := f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false)
		if !errors.Is(err, errDeviceSlow) {
			t.Fatal("live slowdown lost")
		}
		f.store.mu.RLock()
		poll := f.store.devicePoll[digest(code.DeviceCode)]
		f.store.mu.RUnlock()
		if poll.interval != 5*time.Second || !poll.next.Equal(f.now.Add(5*time.Second)) {
			t.Fatal("slowdown interval changed")
		}
	}
	if f.store.SweepExpired() != nil {
		t.Fatal("live sweep")
	}
	pollKeys(t, f.store, 1)
	f.now = f.now.Add(deviceLifetime + time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			f.store.ExchangeDeviceToken(f.project, f.raw, code.DeviceCode, req.ClientID, false)
		}()
		go func() {
			defer wg.Done()
			if f.store.SweepExpired() != nil {
				t.Error("concurrent sweep")
			}
		}()
	}
	wg.Wait()
	pollKeys(t, f.store, 0)
}

func TestDevicePollKeepsOtherLiveState(t *testing.T) {
	f := newDeviceFixture(t)
	polledDevice(t, f, 1)
	f.now = f.now.Add(5 * time.Minute)
	code, _ := polledDevice(t, f, 2)
	f.store.mu.RLock()
	before := f.store.devicePoll[digest(code.DeviceCode)]
	f.store.mu.RUnlock()
	f.now = f.now.Add(6 * time.Minute)
	if f.store.SweepExpired() != nil {
		t.Fatal("sweep")
	}
	pollKeys(t, f.store, 1)
	f.store.mu.RLock()
	after := f.store.devicePoll[digest(code.DeviceCode)]
	f.store.mu.RUnlock()
	if before != after {
		t.Fatal("live rate state changed")
	}
}
func TestDevicePollFailedRemovalKeepsState(t *testing.T) {
	f := newDeviceFixture(t)
	polledDevice(t, f, 1)
	write := f.store.write
	defer func() { f.store.write = write }()
	f.store.write = func([]byte) error { return errors.New("synthetic persistence failure") }
	f.now = f.now.Add(deviceLifetime + time.Second)
	if f.store.SweepExpired() == nil {
		t.Fatal("expected persistence failure")
	}
	pollKeys(t, f.store, 1)
	f.store.write = write
	if f.store.SweepExpired() != nil {
		t.Fatal("retry sweep")
	}
	pollKeys(t, f.store, 0)
}
