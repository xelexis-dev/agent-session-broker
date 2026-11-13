package channel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/localactivity"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func fixtureHarnessHomes(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return home
}
func homeActivityStream(t *testing.T, harness string) (*activityStream, localactivity.Config) {
	t.Helper()
	binding := &ingressBinding{mu: &sync.Mutex{}, identity: broker.AttachRequest{Harness: harness, SessionID: "11111111-1111-4111-8111-111111111111", ChannelID: "lease", PID: 7, CWD: t.TempDir()}, durable: &durableRegistration{clientID: "client", registrationID: "registration"}, generation: 1}
	var selected localactivity.Config
	opts := durableOptions{activityFactory: func(c localactivity.Config) activityObserver {
		c.Host = "fixture-host"
		c.Platform = "fixture-platform"
		c.Probe = func(context.Context, int) (localactivity.Process, error) {
			return localactivity.Process{Key: "fixture-process", Host: c.Host, Platform: c.Platform}, nil
		}
		selected = c
		return localactivity.New(c)
	}}
	stream := opts.newActivityStream(binding)
	return stream, selected
}
func TestActivityObserverCustomHarnessHome(t *testing.T) {
	for _, tc := range []struct{ harness, key string }{{"codex", "CODEX_HOME"}, {"claude", "CLAUDE_CONFIG_DIR"}} {
		t.Run(tc.harness, func(t *testing.T) {
			fixtureHarnessHomes(t)
			root := filepath.Join(t.TempDir(), "custom harness")
			if os.Mkdir(root, 0700) != nil {
				t.Fatal("home fixture failed")
			}
			t.Setenv(tc.key, root)
			stream, cfg := homeActivityStream(t, tc.harness)
			if cfg.Home != root {
				t.Fatal("activity observer ignored custom harness home")
			}
			if tc.harness == "claude" {
				got := stream.observer.Observe(t.Context(), stream.binding)
				if cfg.Format != "" || got.State != localactivity.Unknown || got.Reason != "unsupported" {
					t.Fatal("custom home enabled unsupported Claude activity")
				}
				return
			}
			dir := filepath.Join(root, "sessions", "2026", "11", "12")
			if os.MkdirAll(dir, 0700) != nil {
				t.Fatal("rollout fixture failed")
			}
			meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": cfg.Binding.SessionID, "cwd": cfg.CWD}})
			event, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": "fixture-turn"}})
			path := filepath.Join(dir, "rollout-fixture-"+cfg.Binding.SessionID+".jsonl")
			if os.WriteFile(path, append(append(meta, '\n'), append(event, '\n')...), 0600) != nil {
				t.Fatal("rollout fixture write failed")
			}
			got := stream.observer.Observe(t.Context(), stream.binding)
			if got.State != localactivity.Working {
				t.Fatal("observer did not read custom-root rollout")
			}
		})
	}
}
func TestActivityObserverHomeFallback(t *testing.T) {
	for _, tc := range []struct{ harness, key, dir string }{{"codex", "CODEX_HOME", ".codex"}, {"claude", "CLAUDE_CONFIG_DIR", ".claude"}} {
		for _, mode := range []string{"empty", "unset"} {
			t.Run(tc.harness+"/"+mode, func(t *testing.T) {
				home := fixtureHarnessHomes(t)
				if mode == "unset" {
					if os.Unsetenv(tc.key) != nil {
						t.Fatal("unset fixture failed")
					}
				}
				_, cfg := homeActivityStream(t, tc.harness)
				if cfg.Home != filepath.Join(home, tc.dir) {
					t.Fatal("empty/unset home did not select fixture user home")
				}
			})
		}
	}
}
func TestActivityObserverInvalidHomeUnknown(t *testing.T) {
	for _, harness := range []string{"codex", "claude"} {
		for _, kind := range []string{"relative", "file", "missing-user-home", "relative-user-home"} {
			t.Run(harness+"/"+kind, func(t *testing.T) {
				fixtureHarnessHomes(t)
				key := "CODEX_HOME"
				if harness == "claude" {
					key = "CLAUDE_CONFIG_DIR"
				}
				switch kind {
				case "relative":
					t.Setenv(key, "fixture-relative-home")
				case "file":
					path := filepath.Join(t.TempDir(), "not-a-directory")
					if os.WriteFile(path, nil, 0600) != nil {
						t.Fatal("invalid-home fixture failed")
					}
					t.Setenv(key, path)
				case "missing-user-home":
					t.Setenv("HOME", "")
					t.Setenv("USERPROFILE", "")
				case "relative-user-home":
					t.Setenv("HOME", "fixture-relative-user")
					t.Setenv("USERPROFILE", "fixture-relative-user")
				}
				stream, cfg := homeActivityStream(t, harness)
				if kind != "file" && cfg.Home != "" {
					t.Fatal("invalid root fell back or remained usable")
				}
				got := stream.observer.Observe(t.Context(), stream.binding)
				if got.State != localactivity.Unknown || got.Reason == "" {
					t.Fatal("invalid home did not fail safely as unknown")
				}
			})
		}
	}
}
func TestClaudeResumeCustomHomeShared(t *testing.T) {
	fixtureHarnessHomes(t)
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	id := broker.AttachRequest{Harness: "claude", PID: 123, SessionID: "11111111-1111-4111-8111-111111111111", CWD: "fixture-project"}
	writeFollowSession(t, root, id)
	record := filepath.Join(root, "projects", id.CWD, id.SessionID+".jsonl")
	if os.MkdirAll(filepath.Dir(record), 0700) != nil || os.WriteFile(record, nil, 0600) != nil {
		t.Fatal("record fixture failed")
	}
	data, err := readClaudeSessionFile(id.PID)
	if err != nil || !json.Valid(data) || !claudeRecordExists(id) {
		t.Fatal("Claude resume ignored custom fixture root")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "fixture-relative-home")
	if _, err := readClaudeSessionFile(id.PID); err == nil || claudeRecordExists(id) {
		t.Fatal("Claude resume accepted invalid root")
	}
}

func homeSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skip("Windows symbolic-link privilege unavailable")
		}
		t.Fatal("symlink fixture failed")
	}
}
func writeHomeRollout(t *testing.T, home string, cfg localactivity.Config) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "11", "12")
	if os.MkdirAll(dir, 0700) != nil {
		t.Fatal("rollout fixture failed")
	}
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": cfg.Binding.SessionID, "cwd": cfg.CWD}})
	event, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": "fixture-turn"}})
	path := filepath.Join(dir, "rollout-fixture-"+cfg.Binding.SessionID+".jsonl")
	if os.WriteFile(path, append(append(meta, '\n'), append(event, '\n')...), 0600) != nil {
		t.Fatal("rollout fixture write failed")
	}
	return path
}
func traversalHomeFixture(t *testing.T) (*activityStream, localactivity.Config, string) {
	t.Helper()
	fixtureHarnessHomes(t)
	base := t.TempDir()
	lexical := filepath.Join(base, "lexical")
	physical := filepath.Join(base, "physical")
	if os.MkdirAll(lexical, 0700) != nil || os.MkdirAll(filepath.Join(physical, "child"), 0700) != nil {
		t.Fatal("home fixture failed")
	}
	homeSymlink(t, filepath.Join(physical, "child"), filepath.Join(lexical, "link"))
	t.Setenv("CODEX_HOME", lexical+"/link/../home")
	stream, cfg := homeActivityStream(t, "codex")
	writeHomeRollout(t, filepath.Join(lexical, "home"), cfg)
	path := writeHomeRollout(t, filepath.Join(physical, "home"), cfg)
	return stream, cfg, path
}
func TestReview988SymlinkDotDotHome(t *testing.T) {
	stream, _, _ := traversalHomeFixture(t)
	if got := stream.observer.Observe(t.Context(), stream.binding); got.State != localactivity.Unknown {
		t.Fatal("symlink/parent root bypassed no-follow")
	}
}
func TestReview988SymlinkDotDotAmbiguousHome(t *testing.T) {
	stream, _, path := traversalHomeFixture(t)
	data, err := os.ReadFile(path)
	if err != nil || os.WriteFile(filepath.Join(filepath.Dir(path), "rollout-second-"+stream.binding.SessionID+".jsonl"), data, 0600) != nil {
		t.Fatal("duplicate fixture failed")
	}
	if got := stream.observer.Observe(t.Context(), stream.binding); got.State != localactivity.Unknown {
		t.Fatal("ambiguous physical root was accepted")
	}
}
func TestActivityObserverHomeParentTraversalRefused(t *testing.T) {
	for _, harness := range []string{"codex", "claude"} {
		for _, origin := range []string{"explicit", "fallback"} {
			for _, suffix := range []string{"/child/../home", `\child\..\home`, "/.."} {
				t.Run(harness+"/"+origin+"/"+suffix, func(t *testing.T) {
					fixtureHarnessHomes(t)
					raw := t.TempDir() + suffix
					if origin == "fallback" {
						t.Setenv("HOME", raw)
						t.Setenv("USERPROFILE", raw)
					} else {
						key := "CODEX_HOME"
						if harness == "claude" {
							key = "CLAUDE_CONFIG_DIR"
						}
						t.Setenv(key, raw)
					}
					stream, cfg := homeActivityStream(t, harness)
					if cfg.Home != "" {
						t.Fatal("parent traversal was cleaned or accepted before refusal")
					}
					if got := stream.observer.Observe(t.Context(), stream.binding); got.State != localactivity.Unknown {
						t.Fatal("parent root did not remain unknown")
					}
					if harness == "claude" {
						id := broker.AttachRequest{PID: 123, SessionID: stream.binding.SessionID, CWD: cfg.CWD}
						if _, err := readClaudeSessionFile(id.PID); err == nil || claudeRecordExists(id) {
							t.Fatal("resume accepted parent root")
						}
					}
				})
			}
		}
	}
}
func TestActivityObserverSymlinkHomeUnknown(t *testing.T) {
	fixtureHarnessHomes(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home-link")
	homeSymlink(t, real, link)
	t.Setenv("CODEX_HOME", link)
	stream, cfg := homeActivityStream(t, "codex")
	writeHomeRollout(t, real, cfg)
	plain := cfg
	plain.Home = real
	if got := localactivity.New(plain).Observe(t.Context(), stream.binding); got.State != localactivity.Working {
		t.Fatal("valid root control failed")
	}
	if got := stream.observer.Observe(t.Context(), stream.binding); got.State != localactivity.Unknown {
		t.Fatal("symlink root bypassed existing pathguard")
	}
}
