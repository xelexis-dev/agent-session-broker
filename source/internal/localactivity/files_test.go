package localactivity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLocateRolloutLargeTree(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	start := f.now.AddDate(0, 0, -312)
	for i := 0; i < 4999; i++ {
		dir := filepath.Join(f.config.Home, "sessions", start.AddDate(0, 0, i/16).Format("2006/01/02"))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("rollout-%04d-%s.jsonl", i, sessionB)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := filepath.Rel(f.config.Home, f.path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.observer.locateRollout(context.Background())
	if err != nil || got != want {
		t.Fatalf("5000 rollouts: path=%q err=%v, want %q", got, err, want)
	}
	expectState(t, f.sample(), Working)
}

func TestLocateRolloutDistantDuplicate(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	if _, err := f.observer.locateRollout(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.config.Home, "sessions", f.now.AddDate(0, 0, -4).Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(f.path)), []byte(f.meta(sessionA)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.observer.locateRollout(context.Background()); err != mismatch {
		t.Fatalf("duplicate UUID four days away: err=%v, want %v", err, mismatch)
	}
}

func TestLocateRolloutDirectoryEntryBudget(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA))
	for i := 0; i < 4095; i++ {
		if err := os.WriteFile(filepath.Join(filepath.Dir(f.path), fmt.Sprintf("ignored-%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.observer.locateRollout(context.Background()); err != nil {
		t.Fatalf("4096 entries must fit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.path), "one-too-many"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.observer.locateRollout(context.Background()); err != budgetExceeded {
		t.Fatalf("4097 entries: err=%v, want %v", err, budgetExceeded)
	}
}

func TestLocateRolloutDateDirectoryBudget(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA))
	for i := 1; i < 4096; i++ {
		dir := filepath.Join(f.config.Home, "sessions", f.now.AddDate(0, 0, -i).Format("2006/01/02"))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.observer.locateRollout(context.Background()); err != nil {
		t.Fatalf("4096 date directories must fit: %v", err)
	}
	dir := filepath.Join(f.config.Home, "sessions", f.now.AddDate(0, 0, -4096).Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.observer.locateRollout(context.Background()); err != budgetExceeded {
		t.Fatalf("4097 date directories: err=%v, want %v", err, budgetExceeded)
	}
}

func TestLocateRolloutCancelled(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.observer.locateRollout(ctx); err != unavailable {
		t.Fatalf("cancelled context: err=%v, want %v", err, unavailable)
	}
}

func TestLocateRolloutNonRegularTarget(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, "codex")
			if kind == "directory" {
				if err := os.Mkdir(f.path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(f.path+"-real", []byte(f.meta(sessionA)), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.path+"-real", f.path); err != nil {
					t.Skip("symlinks unavailable")
				}
			}
			if _, err := f.observer.locateRollout(context.Background()); err != malformed {
				t.Fatalf("%s target: err=%v, want %v", kind, err, malformed)
			}
		})
	}
}
