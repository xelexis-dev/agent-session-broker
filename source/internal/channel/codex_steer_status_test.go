//go:build darwin || linux

package channel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestCodexSteerStatusUncheckedReadOnly(t *testing.T) {
	f := &fakeSteerControl{}
	r, pushed, completed := steerFixture(t, f)
	if _, err := writeCodexHook(r, "Stop", "T"); err != nil {
		t.Fatal(err)
	}
	r.steer.dial = func(context.Context) (codexControl, error) { t.Fatal("status dialed control"); return nil, nil }
	r.steer.diagnose = func(string) { t.Fatal("status recorded a diagnostic") }
	generation := r.generation
	for range 2 {
		if got := r.deliveryStatus(); *got != (codexDelivery{Mode: "hook", Reason: "steer-unchecked"}) {
			t.Errorf("idle status = %+v", got)
		}
	}
	if len(f.calls) != 0 || r.steer.reason != "" || r.steer.disabled || r.steer.working || r.steer.again || r.steer.timer != nil || !r.steer.due.IsZero() || r.steer.batch != nil || r.generation != generation || *pushed != 0 || *completed != 0 {
		t.Fatal("status read changed delivery state")
	}
}

func TestCodexSteerStatusStaleGate(t *testing.T) {
	f := &fakeSteerControl{}
	r, _, completed := steerFixture(t, f)
	gate := "unconfirmed-thread"
	r.steer.gate = func() string { return gate }
	if err := steerMessage(r, completed, 1); err != nil {
		t.Fatal(err)
	}
	if r.steer.reason != gate || len(f.calls) != 0 {
		t.Fatal("fixture did not retain the closed gate")
	}
	gate = ""
	if got := r.deliveryStatus(); *got != (codexDelivery{Mode: "hook", Reason: "steer-unchecked"}) {
		t.Fatalf("cleared gate status = %+v", got)
	}
	if r.steer.reason != "unconfirmed-thread" || len(f.calls) != 0 {
		t.Fatal("status wrote stale reason or dialed control")
	}
}

func TestCodexSteerStatusRegression(t *testing.T) {
	for _, tc := range []struct {
		name, gate, reason, owner, mode, want string
		disabled, batch                       bool
		recovery                              *steerRecovery
	}{
		{name: "off", gate: "off", reason: "steer-ready", mode: "hook", want: "off"},
		{name: "unconfirmed", gate: "unconfirmed-thread", mode: "hook", want: "unconfirmed-thread"},
		{name: "ack", gate: "ack-required", disabled: true, mode: "hook", want: "ack-required"},
		{name: "verification-failed", reason: "unverified-host", mode: "hook", want: "unverified-host"},
		{name: "ready", reason: "steer-ready", mode: "steer", want: "steer-ready"},
		{name: "disabled", reason: "steer-ready", disabled: true, mode: "hook", want: "unsupported-codex"},
		{name: "uncertain", gate: "off", reason: "steer-uncertain", owner: "steer-uncertain", batch: true, disabled: true, mode: "steer", want: "steer-uncertain"},
		{name: "recovery", gate: "off", owner: "steer-uncertain", batch: true, recovery: &steerRecovery{}, mode: "steer", want: "steer-recovery-pending"},
		{name: "unavailable", gate: "off", recovery: &steerRecovery{loaded: true, unavailable: true}, mode: "steer", want: "steer-recovery-unavailable"},
		{name: "stale-ack", reason: "ack-required", mode: "hook", want: "steer-unchecked"},
		{name: "stale-uncertain", reason: "steer-uncertain", mode: "hook", want: "steer-unchecked"},
		{name: "stale-recovery", reason: "steer-recovery-pending", mode: "hook", want: "steer-unchecked"},
		{name: "stale-unavailable", reason: "steer-recovery-unavailable", mode: "hook", want: "steer-unchecked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSteerControl{}
			r, _, _ := steerFixture(t, f)
			r.steer.gate = func() string { return tc.gate }
			r.steer.reason = tc.reason
			r.steer.disabled = tc.disabled
			r.owner = tc.owner
			r.recovery = tc.recovery
			if tc.batch {
				r.steer.batch = &steerBatch{}
			}
			if got := r.deliveryStatus(); *got != (codexDelivery{Mode: tc.mode, Reason: tc.want}) {
				t.Errorf("status = %+v, want %s/%s", got, tc.mode, tc.want)
			}
			if r.steer.reason != tc.reason || len(f.calls) != 0 {
				t.Fatal("status changed reason or sent control RPC")
			}
		})
	}
}

func TestCodexSteerStatusNilOmitted(t *testing.T) {
	for _, router := range []*codexIngress{nil, newCodexIngress(nil)} {
		out := claimAuthStatus(&ingressBinding{mu: &sync.Mutex{}, router: router}, authStatus{})
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if out.CodexDelivery != nil || strings.Contains(string(raw), "codexDelivery") {
			t.Fatal("absent steerer exposed delivery status")
		}
	}
}

func TestCodexSteerStatusUncheckedTransitions(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "verification-failed"}[fail], func(t *testing.T) {
			f := &fakeSteerControl{}
			r, _, completed := steerFixture(t, f)
			if got := r.deliveryStatus(); got.Reason != "steer-unchecked" {
				t.Errorf("initial status = %+v", got)
			}
			if fail {
				r.steer.dial = func(context.Context) (codexControl, error) { return nil, context.DeadlineExceeded }
			}
			if err := steerMessage(r, completed, 1); err != nil {
				t.Fatal(err)
			}
			want := codexDelivery{Mode: "steer", Reason: "steer-ready"}
			if fail {
				want = codexDelivery{Mode: "hook", Reason: "unverified-host"}
			}
			if got := r.deliveryStatus(); *got != want {
				t.Fatalf("after attempt status = %+v, want %+v", got, want)
			}
		})
	}
}

func TestCodexSteerStatusAfterGenerationRecovery(t *testing.T) {
	f := newRecoveryFixture(t, true)
	f.r.steer.gate = func() string { return "" }
	f.r.steer.reason = "steer-uncertain"
	f.r.steer.batch = &steerBatch{}
	f.r.owner = "steer-uncertain"
	if got := f.r.deliveryStatus(); got.Reason != "steer-uncertain" {
		t.Fatalf("initial uncertain status = %+v", got)
	}
	f.r.nextGeneration()
	if got := f.r.deliveryStatus(); got.Reason != "steer-recovery-pending" {
		t.Fatalf("generation recovery status = %+v", got)
	}
	f.visible = true
	f.deliver(t, 1)
	f.deliver(t, 2)
	if !f.r.recovery.resolved || f.r.owner != "" {
		t.Fatal("fixture recovery not resolved")
	}
	if got := f.r.deliveryStatus(); *got != (codexDelivery{Mode: "hook", Reason: "steer-unchecked"}) {
		t.Fatalf("resolved recovery status = %+v", got)
	}
	if f.r.steer.reason != "steer-uncertain" {
		t.Fatal("status changed stored recovery reason")
	}
}
