package localactivity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fastFixture(t *testing.T) *fixture {
	f := newFixture(t, "codex")
	f.config.Format = CodexTaskPairV1
	f.observer = New(f.config)
	return f
}

func TestObserveFastCommittedTail(t *testing.T) {
	for _, kind := range []string{"missing", "pair", "future"} {
		t.Run(kind, func(t *testing.T) {
			f := fastFixture(t)
			if kind == "missing" {
				f.config.Format = CodexRolloutV1
				f.observer = New(f.config)
			}
			tail := `{"type":"response_item","payload":{"type":"message","content":"` + strings.Repeat("x", 16000) + `"}}` + "\n"
			if kind == "pair" {
				tail += f.event("unexpected", "old")
			}
			if kind == "future" {
				f.now = f.now.Add(2 * time.Minute)
				tail += f.event("task_started", "old")
				f.now = f.now.Add(-2 * time.Minute)
			}
			f.write(f.meta(sessionA) + tail)
			got := f.sample()
			if got.State != Unknown {
				t.Fatal(got)
			}
			if kind == "missing" && got.Reason != string(missing) {
				t.Fatalf("missing fixture: %+v", got)
			}
			before := f.observer.rollout.offset
			if before == 0 {
				t.Error("committed tail offset lost")
			}
			budget := &readBudget{ctx: context.Background(), left: 1 << 20}
			_, _, err := f.observer.observeCodex(budget, f.now)
			if used := int64(1<<20) - budget.left; used > 1024+anchorLimit {
				t.Errorf("unchanged tail read %d bytes; want <= metadata+anchor", used)
			}
			if err == nil {
				t.Fatal("unknown tail became valid")
			}
			f.now = f.now.Add(3 * time.Minute)
			if kind == "future" {
				if got := f.sample(); got.Reason != string(malformed) {
					t.Fatalf("clock alone cleared future reason: %+v", got)
				}
			}
			f.append(f.event("task_started", "fresh"))
			expectState(t, f.sample(), Working)
			f.append(f.event("task_complete", "fresh"))
			expectState(t, f.sample(), Idle)
		})
	}
}

func TestObserveFastReadFailuresReset(t *testing.T) {
	for _, kind := range []string{"partial", "io", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := fastFixture(t)
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			expectState(t, f.sample(), Working)
			switch kind {
			case "partial":
				f.append(`{"type":`)
			case "io":
				if err := os.Remove(f.path); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				calls := 0
				f.observer.config.Probe = func(ctx context.Context, pid int) (Process, error) {
					calls++
					if calls == 2 {
						return Process{}, unavailable
					}
					return f.process, nil
				}
			}
			if got := f.sample(); got.State != Unknown {
				t.Fatal(got)
			}
			if f.observer.rollout.offset != 0 || f.observer.rollout.anchorLen != 0 {
				t.Fatal("read/probe error retained offset/anchor")
			}
		})
	}
}

func distantFile(t *testing.T, f *fixture, days int) string {
	t.Helper()
	p := filepath.Join(f.config.Home, "sessions", f.now.AddDate(0, 0, -days).Format("2006/01/02"), filepath.Base(f.path))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(f.meta(sessionA)), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestObserveFastDistantDuplicateRevalidation(t *testing.T) {
	f := fastFixture(t)
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	expectState(t, f.sample(), Working)
	dup := distantFile(t, f, 4)
	f.now = f.now.Add(59 * time.Second)
	expectState(t, f.sample(), Working)
	f.now = f.now.Add(time.Second)
	if got := f.sample(); got.State != Unknown || got.Reason != string(mismatch) {
		t.Fatalf("distant duplicate not detected: %+v", got)
	}
	for range 3 {
		f.now = f.now.Add(5 * time.Second)
		if got := f.sample(); got.State != Unknown || got.Reason != string(mismatch) {
			t.Fatalf("known duplicate became healthy: %+v", got)
		}
	}
	if err := os.Remove(dup); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	if got := f.sample(); got.State != Unknown {
		t.Fatalf("recent scan restored invalid uniqueness: %+v", got)
	}
	f.now = f.now.Add(60 * time.Second)
	expectState(t, f.sample(), Working)
}

func TestObserveFastFullFailureRevalidation(t *testing.T) {
	f := fastFixture(t)
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	expectState(t, f.sample(), Working)
	bad := filepath.Join(f.config.Home, "sessions", "1999")
	if err := os.Symlink(f.config.CWD, bad); err != nil {
		t.Skip("symlink unavailable")
	}
	f.now = f.now.Add(59 * time.Second)
	expectState(t, f.sample(), Working)
	f.now = f.now.Add(time.Second)
	if got := f.sample(); got.State != Unknown {
		t.Fatal("failed full scan kept cached uniqueness")
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	if got := f.sample(); got.State != Unknown {
		t.Fatal("recent scan restored failed full scan")
	}
	f.now = f.now.Add(60 * time.Second)
	expectState(t, f.sample(), Working)
}

func TestObserveFastRecentMatches(t *testing.T) {
	for _, days := range []int{0, 1} {
		t.Run(time.Duration(days).String(), func(t *testing.T) {
			f := fastFixture(t)
			f.now = f.now.In(time.Local)
			f.path = filepath.Join(f.config.Home, "sessions", f.now.Format("2006/01/02"), filepath.Base(f.path))
			if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
				t.Fatal(err)
			}
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			expectState(t, f.sample(), Working)
			f.now = f.now.Add(time.Second)
			expectState(t, f.sample(), Working)
			if days == 0 {
				dup := f.path + "-other-" + sessionA + ".jsonl"
				if err := os.WriteFile(dup, []byte(f.meta(sessionA)), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				distantFile(t, f, days)
			}
			if got := f.sample(); got.State != Unknown {
				t.Fatalf("recent duplicate not detected: %+v", got)
			}
		})
	}
}

func TestObserveFastSameFileAlias(t *testing.T) {
	f := fastFixture(t)
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	expectState(t, f.sample(), Working)
	// A hard link is the same evidence, including during full revalidation.
	alias := filepath.Join(filepath.Dir(f.path), "rollout-aaa-"+sessionA+".jsonl")
	if err := os.Link(f.path, alias); err != nil {
		t.Skip("hard links unavailable")
	}
	expectState(t, f.sample(), Working)
	f.now = f.now.Add(time.Minute)
	expectState(t, f.sample(), Working)
}

func TestObserveFastMissingBackoff(t *testing.T) {
	f := fastFixture(t)
	if got := f.sample(); got.Reason != string(missing) {
		t.Fatal(got)
	}
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	f.now = f.now.Add(4 * time.Second)
	if got := f.sample(); got.State != Unknown {
		t.Fatal("retried missing before 5 seconds")
	}
	f.now = f.now.Add(time.Second)
	expectState(t, f.sample(), Working)
}

func TestObserveFastMissingBackoffCaps(t *testing.T) {
	f := fastFixture(t)
	for _, delay := range []time.Duration{5, 10, 20, 40, 60, 60} {
		if got := f.sample(); got.Reason != string(missing) {
			t.Fatal(got)
		}
		// Place valid evidence just before each retry; it must stay unknown until due.
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		f.now = f.now.Add(delay*time.Second - time.Nanosecond)
		if got := f.sample(); got.State != Unknown {
			t.Fatal("early missing retry")
		}
		if err := os.Remove(f.path); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(time.Nanosecond)
	}
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	expectState(t, f.sample(), Working)
}
