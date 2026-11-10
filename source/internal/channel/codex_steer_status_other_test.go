//go:build !darwin && !linux

package channel

import (
	"context"
	"testing"
)

func TestCodexSteerStatusNonPOSIXUnchecked(t *testing.T) {
	r, _, _ := steerFixture(t, &fakeSteerControl{})
	r.steer.dial = func(context.Context) (codexControl, error) { t.Fatal("status dialed stub"); return nil, nil }
	for _, reason := range []string{"", "unconfirmed-thread", "ack-required", "steer-uncertain", "steer-recovery-pending", "steer-recovery-unavailable", "unverified-host"} {
		r.steer.reason = reason
		if got := r.deliveryStatus(); *got != (codexDelivery{Mode: "hook", Reason: "unverified-host"}) {
			t.Errorf("stored %q: status = %+v", reason, got)
		}
		if r.steer.reason != reason {
			t.Fatal("status changed stored reason")
		}
	}
}
