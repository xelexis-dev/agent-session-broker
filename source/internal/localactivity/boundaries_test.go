package localactivity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func expectState(t *testing.T, got Observation, state string) {
	t.Helper()
	if got.State != state {
		t.Fatalf("state=%s reason=%s, want %s", got.State, got.Reason, state)
	}
}

func TestBindingAndProcessIsolation(t *testing.T) {
	for _, h := range []string{"claude", "codex"} {
		t.Run(h, func(t *testing.T) {
			f := newFixture(t, h)
			if h == "claude" {
				f.write(f.claude("busy", sessionA))
			} else {
				f.write(f.meta(sessionA) + f.event("task_started", "a"))
			}
			expectState(t, f.sample(), Working)
			f.process.Key = "reused-pid"
			expectState(t, f.sample(), Unknown)
			f.process.Key = "creation-a"
			expectState(t, f.sample(), Unknown) // A reused PID cannot revive the old observer.
		})
	}
	for _, field := range []string{"session", "client", "registration", "lease", "generation"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t, "codex")
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			b := f.config.Binding
			switch field {
			case "session":
				b.SessionID = sessionB
			case "client":
				b.ClientID = "other"
			case "registration":
				b.RegistrationID = "other"
			case "lease":
				b.LeaseID = "other"
			case "generation":
				b.Generation++
			}
			expectState(t, f.observer.Observe(context.Background(), b), Unknown)
			expectState(t, f.sample(), Unknown)
		})
	}
}

func TestSharedPIDKeepsConversationsSeparate(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	other := f.config
	other.Binding.SessionID = sessionB
	other.Binding.RegistrationID = "other-registration"
	pathB := strings.Replace(f.path, sessionA, sessionB, 1)
	b, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": sessionB, "cwd": f.config.CWD}})
	if err := os.WriteFile(pathB, append(b, []byte("\n"+f.event("task_started", "b")+f.event("task_complete", "b"))...), 0600); err != nil {
		t.Fatal(err)
	}
	expectState(t, f.sample(), Working)
	expectState(t, New(other).Observe(context.Background(), other.Binding), Idle)
}

func TestIdentityMetadataMustMatch(t *testing.T) {
	for _, h := range []string{"claude", "codex"} {
		for _, field := range []string{"cwd", "host", "creation", "missing-creation"} {
			t.Run(h+"/"+field, func(t *testing.T) {
				f := newFixture(t, h)
				if h == "claude" {
					f.write(f.claude("busy", sessionA))
				} else {
					f.write(f.meta(sessionA) + f.event("task_started", "a"))
				}
				switch field {
				case "cwd":
					f.config.CWD = t.TempDir()
				case "host":
					f.config.Host = "other-host"
				case "creation":
					f.process.RecordStart = "reused"
				case "missing-creation":
					f.process.RecordStart = ""
				}
				f.observer = New(f.config)
				if h == "codex" && (field == "creation" || field == "missing-creation") {
					f.process.Key = ""
				}
				expectState(t, f.sample(), Unknown)
			})
		}
	}
	t.Run("codex-meta-host", func(t *testing.T) {
		f := newFixture(t, "codex")
		f.write(strings.Replace(f.meta(sessionA), `"cwd":`, `"hostname":"other-host","cwd":`, 1) + f.event("task_started", "a"))
		expectState(t, f.sample(), Unknown)
	})
	for _, field := range []string{"pid", "pidDomain", "procStart", "kind", "status", "statusUpdatedAt"} {
		t.Run("claude/"+field, func(t *testing.T) {
			f := newFixture(t, "claude")
			var d map[string]any
			json.Unmarshal([]byte(f.claude("busy", sessionA)), &d)
			if field == "pid" || field == "statusUpdatedAt" {
				d[field] = 0
			} else {
				d[field] = "unsupported"
			}
			b, _ := json.Marshal(d)
			f.write(string(b))
			expectState(t, f.sample(), Unknown)
		})
	}
}

func TestMissingAmbiguousAndSymlinkFiles(t *testing.T) {
	for _, h := range []string{"claude", "codex"} {
		for _, kind := range []string{"missing", "file-link", "parent-link", "root-link"} {
			t.Run(h+"/"+kind, func(t *testing.T) {
				f := newFixture(t, h)
				if kind == "missing" {
					expectState(t, f.sample(), Unknown)
					return
				}
				if h == "claude" {
					f.write(f.claude("busy", sessionA))
				} else {
					f.write(f.meta(sessionA) + f.event("task_started", "a"))
				}
				target := f.path
				if kind == "parent-link" {
					target = filepath.Dir(f.path)
				}
				if kind == "root-link" {
					target = f.config.Home
				}
				if err := os.Rename(target, target+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target+"-real", target); err != nil {
					t.Skip("symlinks unavailable")
				}
				expectState(t, f.sample(), Unknown)
			})
		}
	}
	t.Run("ambiguous", func(t *testing.T) {
		f := newFixture(t, "codex")
		data := f.meta(sessionA) + f.event("task_started", "a")
		f.write(data)
		expectState(t, f.sample(), Working)
		other := filepath.Join(filepath.Dir(f.path), "rollout-other-"+sessionA+".jsonl")
		if err := os.WriteFile(other, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		expectState(t, f.sample(), Unknown)
	})
}

func TestFileReplacementTruncationAndRewrite(t *testing.T) {
	for _, kind := range []string{"replace", "truncate", "rewrite"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, "codex")
			data := f.meta(sessionA) + f.event("task_started", "a")
			f.write(data)
			expectState(t, f.sample(), Working)
			switch kind {
			case "replace":
				if err := os.WriteFile(f.path+"-replacement", []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(f.path+"-replacement", f.path); err != nil {
					t.Fatal(err)
				}
			case "truncate":
				f.write(f.meta(sessionA))
			case "rewrite":
				f.write(strings.Replace(data, `"turn_id":"a"`, `"turn_id":"b"`, 1))
			}
			expectState(t, f.sample(), Unknown)
		})
	}
	t.Run("claude-replace", func(t *testing.T) {
		f := newFixture(t, "claude")
		f.write(f.claude("busy", sessionA))
		expectState(t, f.sample(), Working)
		if err := os.Rename(f.path, f.path+"-old"); err != nil {
			t.Fatal(err)
		}
		f.write(f.claude("idle", sessionA))
		expectState(t, f.sample(), Unknown)
	})
}

func TestCodexTurnBoundaries(t *testing.T) {
	for _, kind := range []string{"end-only", "another-end", "empty-turn", "unknown-lifecycle", "partial", "corrupt", "oversized", "out-of-order"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, "codex")
			data := f.meta(sessionA) + f.event("task_started", "a")
			switch kind {
			case "end-only":
				data = f.meta(sessionA) + f.event("task_complete", "a")
			case "another-end":
				data += f.event("task_complete", "b")
			case "empty-turn":
				data += f.event("task_complete", "")
			case "unknown-lifecycle":
				data += f.event("turn_new_semantics", "a")
			case "partial":
				data += strings.TrimSuffix(f.event("task_complete", "a"), "\n")
			case "corrupt":
				data += "{bad}\n"
			case "oversized":
				data += `{"type":"response_item","unused":"` + strings.Repeat("x", 1<<20) + "\"}\n"
			case "out-of-order":
				f.now = f.now.Add(-time.Second)
				data += f.event("task_complete", "a")
			}
			f.write(data)
			expectState(t, f.sample(), Unknown)
		})
	}
	t.Run("partial-completed-next-poll", func(t *testing.T) {
		f := newFixture(t, "codex")
		f.write(f.meta(sessionA) + f.event("task_started", "a") + strings.TrimSuffix(f.event("task_complete", "a"), "\n"))
		expectState(t, f.sample(), Unknown)
		f.append("\n")
		expectState(t, f.sample(), Idle)
	})
	t.Run("interrupt", func(t *testing.T) {
		f := newFixture(t, "codex")
		f.write(f.meta(sessionA) + f.event("task_started", "a") + f.event("turn_aborted", "a"))
		expectState(t, f.sample(), Idle)
	})
}

func TestStaleEvidenceIsNotRefreshedByRereadOrMessage(t *testing.T) {
	for _, h := range []string{"claude", "codex"} {
		t.Run(h, func(t *testing.T) {
			f := newFixture(t, h)
			if h == "claude" {
				f.write(f.claude("busy", sessionA))
			} else {
				f.write(f.meta(sessionA) + f.event("task_started", "a"))
			}
			expectState(t, f.sample(), Working)
			f.now = f.now.Add(5*time.Minute - time.Nanosecond)
			expectState(t, f.sample(), Working)
			if h == "codex" {
				f.append(f.event("agent_message", "a"))
			}
			f.now = f.now.Add(time.Nanosecond)
			expectState(t, f.sample(), Unknown)
			expectState(t, f.sample(), Unknown)
		})
	}
	t.Run("already-stale-first-observation", func(t *testing.T) {
		f := newFixture(t, "codex")
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		f.now = f.now.Add(6 * time.Minute)
		expectState(t, f.sample(), Unknown)
	})
	t.Run("matching-tool-metadata", func(t *testing.T) {
		f := newFixture(t, "codex")
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		expectState(t, f.sample(), Working)
		f.now = f.now.Add(4 * time.Minute)
		f.append(f.event("tool_complete", "a"))
		expectState(t, f.sample(), Working)
		f.now = f.now.Add(4 * time.Minute)
		expectState(t, f.sample(), Working)
	})
}

func TestReadBudgets(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		f := newFixture(t, "claude")
		f.write(f.claude("busy", sessionA) + strings.Repeat(" ", 64<<10))
		got := f.sample()
		expectState(t, got, Unknown)
		if got.Reason != "budget-exceeded" {
			t.Fatal(got.Reason)
		}
	})
	t.Run("incremental", func(t *testing.T) {
		f := newFixture(t, "codex")
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		expectState(t, f.sample(), Working)
		f.append(strings.Repeat(f.event("agent_message", "a"), 12000))
		got := f.sample()
		expectState(t, got, Unknown)
		if got.Reason != "budget-exceeded" {
			t.Fatal(got.Reason)
		}
	})
	t.Run("large-tail", func(t *testing.T) {
		f := newFixture(t, "codex")
		f.write(f.meta(sessionA) + strings.Repeat(f.event("agent_message", "a"), 80000) + f.event("task_started", "a"))
		expectState(t, f.sample(), Working)
		f.append(f.event("task_complete", "a"))
		expectState(t, f.sample(), Idle)
	})
	t.Run("initial-tail-without-start", func(t *testing.T) {
		f := newFixture(t, "codex")
		f.write(f.meta(sessionA) + f.event("task_started", "a") + strings.Repeat(f.event("agent_message", "a"), 80000) + f.event("task_complete", "a"))
		expectState(t, f.sample(), Unknown)
	})
	t.Run("entry-budget", func(t *testing.T) {
		f := newFixture(t, "codex")
		f.write(f.meta(sessionA) + f.event("task_started", "a"))
		for i := 0; i < maxEntries; i++ {
			name := filepath.Join(filepath.Dir(f.path), time.Unix(int64(i), 0).Format("150405"))
			if err := os.WriteFile(name, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		got := f.sample()
		expectState(t, got, Unknown)
		if got.Reason != "budget-exceeded" {
			t.Fatal(got.Reason)
		}
	})
}

func TestUnknownDefaultCancellationAndConcurrentSequences(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	c := f.config
	c.Format = ""
	got := New(c).Observe(context.Background(), c.Binding)
	expectState(t, got, Unknown)
	if got.Reason != "unsupported" {
		t.Fatal(got.Reason)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	expectState(t, f.observer.Observe(ctx, f.config.Binding), Unknown)
	var wg sync.WaitGroup
	results := make(chan uint64, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- f.sample().Sequence }()
	}
	wg.Wait()
	close(results)
	seen := map[uint64]bool{}
	for n := range results {
		if seen[n] || n < 2 || n > 17 {
			t.Fatalf("invalid sequence %d", n)
		}
		seen[n] = true
	}
}

func TestRelativeHomeAndMissingPIDAreNotEvidenceRoots(t *testing.T) {
	for _, field := range []string{"home", "pid"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t, "codex")
			f.write(f.meta(sessionA) + f.event("task_started", "a"))
			if field == "home" {
				f.config.Home = "."
			} else {
				f.config.PID = 0
			}
			got := New(f.config).Observe(context.Background(), f.config.Binding)
			expectState(t, got, Unknown)
			if got.Reason != "identity-mismatch" {
				t.Fatal("invalid local configuration accepted")
			}
		})
	}
}

func TestCancellationDuringReadDoesNotPoisonTheBinding(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	f.config.Probe = func(context.Context, int) (Process, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return f.process, nil
	}
	f.observer = New(f.config)
	expectState(t, f.observer.Observe(ctx, f.config.Binding), Unknown)
	expectState(t, f.sample(), Working)
}

func TestProcessReplacementDuringObservationIsRejected(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + f.event("task_started", "a"))
	calls := 0
	f.config.Probe = func(context.Context, int) (Process, error) {
		calls++
		p := f.process
		if calls > 1 {
			p.Key = "changed-during-read"
		}
		return p, nil
	}
	f.observer = New(f.config)
	expectState(t, f.sample(), Unknown)
}

func TestObservationDoesNotExposeRawEvidenceAndClosesFiles(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + f.event("task_started", "a") + `{"type":"response_item","payload":{"type":"message","body":"SYNTHETIC-SECRET-MARKER"}}` + "\n")
	before, err := os.ReadDir("/dev/fd")
	for range 100 {
		out := f.sample()
		expectState(t, out, Working)
		b, _ := json.Marshal(out)
		for _, secret := range []string{"SYNTHETIC-SECRET-MARKER", f.config.CWD, f.config.Home, sessionA, "turn_id"} {
			if strings.Contains(string(b), secret) {
				t.Fatal("sample leaked evidence")
			}
		}
	}
	if err == nil {
		f.write("{bad}\n")
		for range 100 {
			expectState(t, New(f.config).Observe(context.Background(), f.config.Binding), Unknown)
		}
		after, e := os.ReadDir("/dev/fd")
		if e == nil && len(after) > len(before)+1 {
			t.Fatalf("descriptor growth before=%d after=%d", len(before), len(after))
		}
	}
}
