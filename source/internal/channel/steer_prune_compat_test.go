package channel

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Run this file unchanged against tag 0.17.3: simulate the new collector's
// completed unlink, then exercise only the old client's recovery.
func TestSteerPrunedCachedEmptyHookCompatibility(t *testing.T) {
	f := newRecoveryFixture(t, false)
	before, err := f.store.LoadSteerAttempt("scope", recoveryThread)
	if err != nil {
		t.Fatal(err)
	}
	bound := before
	bound.Registration, bound.Epoch = "R", "E"
	if err := f.store.SaveSteerAttempt("scope", recoveryThread, before, bound); err != nil {
		t.Fatal(err)
	}
	f.r.recovery.loaded = false
	f.r.initSteerRecovery()
	if !reflect.DeepEqual(f.r.recovery.record, bound) {
		t.Fatal("bound empty record not cached")
	}
	files, err := filepath.Glob(filepath.Join(f.store.Dir(), "steer-attempts", "v1", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("fixture record missing")
	}
	// No reader/writer is in flight during the collector's simulated unlink.
	for _, path := range []string{files[0], files[0] + ".lock"} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	native := &fakeSteerControl{}
	f.r.steer.gate = func() string { return "" }
	f.r.steer.delay = time.Hour
	f.r.steer.dial = func(context.Context) (codexControl, error) { return native, nil }
	writeCodexHook(f.r, "PreToolUse", "T")
	f.deliver(t, 1)
	if !f.r.recovery.unavailable || f.r.steer.batch == nil || f.r.steer.batch.rpcSent {
		t.Fatal("stale cached CAS did not hold before RPC")
	}
	f.deliver(t, 2)
	if err := f.r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.pushed) != 0 {
		t.Fatal("hook messages escaped through queue")
	}
	expected, err := codexMessageContent(scanMessages())
	if err != nil {
		t.Fatal(err)
	}
	out, done, failed, err := f.r.hookHandoff("PostToolUse", "T")
	if failed != nil {
		defer failed()
	}
	if err != nil || done == nil {
		t.Fatal("reloaded noSend batch did not reach hook")
	}
	body, ok := out["hookSpecificOutput"].(map[string]any)
	if !ok || body["additionalContext"] != expected {
		t.Fatal("hook delivery not in-order exactly once")
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
	again, _, _, err := f.r.hookHandoff("PostToolUse", "T")
	if err != nil || len(again) != 0 {
		t.Fatal("hook message delivered twice")
	}
	if through, _ := f.state.snapshot(); through != 2 {
		t.Fatal("hook completion did not acknowledge ordered prefix")
	}
	if len(f.pushed) != 0 || native.inputs != 0 {
		t.Fatal("pruned cached empty record sent turn/steer or queue input")
	}
	record, err := f.store.LoadSteerAttempt("scope", recoveryThread)
	if err != nil || record.Pending != nil {
		t.Fatal("cleared record not recreated")
	}
	if _, err := os.Stat(files[0]); err != nil {
		t.Fatal("record not persisted again")
	}
	if record.Format != 1 {
		t.Fatal("record format changed")
	}
}
