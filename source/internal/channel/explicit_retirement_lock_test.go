package channel

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// A separate test process bounds a real self-deadlock, including blocked cleanup.
func TestExplicitRetirementDiagnosesOutsideBindingLock(t *testing.T) {
	if mode := os.Getenv("ASB_X1_LOCK_CASE"); mode != "" {
		explicitRetirementLockCase(t, mode)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"registration", "epoch"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestExplicitRetirementDiagnosesOutsideBindingLock$", "-test.timeout=4s")
			home, tmp := t.TempDir(), t.TempDir()
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SYSTEMROOT=" + os.Getenv("SYSTEMROOT"), "HOME=" + home, "USERPROFILE=" + home, "TMPDIR=" + tmp, "TEMP=" + tmp, "TMP=" + tmp, "ASB_X1_LOCK_CASE=" + mode}
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			err := cmd.Run()
			free := strings.Contains(output.String(), "LOCK_CALLBACK_FREE=true")
			if err != nil || !free {
				t.Fatalf("bounded real-diagnose check failed: lockFree=%t childSucceeded=%t timedOut=%t", free, err == nil, ctx.Err() != nil || strings.Contains(output.String(), "test timed out"))
			}
		})
	}
}

func explicitRetirementLockCase(t *testing.T, mode string) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 1890)
	manager.attach(t)
	made := make(chan *ingressBinding, 1)
	var router *codexIngress
	stateDir := t.TempDir()
	h := r.harness(t, stateDir, 1891, func(o *durableOptions) {
		o.bindingCreated = func(b *ingressBinding) { made <- b }
		o.claudeInboxFactory = func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			router = newCodexIngress(func(context.Context, broker.Message) error { return nil })
			router.steer = &codexSteerer{}
			return func(context.Context, broker.Message) error { return nil }, router, nil
		}
	})
	h.attach(t)
	b := <-made
	old := h.session
	if mode == "registration" {
		if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": old.RegistrationID}, nil); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "retirement detached", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.failed && b.durable.detached })
		cleanupPanelList(t, r)
	} else {
		b.mu.Lock()
		cancel, done := b.cancel, b.done
		b.mu.Unlock()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("old receiver not joined")
		}
	}
	router.mu.Lock()
	epoch := old.Epoch
	if mode == "epoch" {
		epoch = "prior-epoch"
	}
	router.steer.batch = &steerBatch{registration: old.RegistrationID, epoch: epoch, generation: router.generation, messages: []broker.Message{{DeliverySequence: 1, Body: "old uncertain"}}}
	router.owner = "steer-uncertain"
	realDiagnose := router.steer.diagnose
	router.steer.diagnose = func(code string) {
		free := b.mu.TryLock()
		if free {
			b.mu.Unlock()
		}
		fmt.Printf("LOCK_CALLBACK_FREE=%t\n", free)
		if !free {
			t.Error("retirement diagnose called under binding lock")
		}
		realDiagnose(code)
	}
	router.mu.Unlock()
	h.attach(t)
	if (mode == "registration") != (h.session.RegistrationID != old.RegistrationID) {
		t.Fatal("wrong registration transition")
	}
	store, err := clientstate.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.LoadDiagnostics(b.durable.key)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range d.Events {
		if e.Event == "steer-uncertain-retired" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("real v2 retirement diagnostic missing or duplicated")
	}
}
