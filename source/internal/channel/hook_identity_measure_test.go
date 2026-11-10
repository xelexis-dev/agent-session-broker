package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/codex"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Opt-in metadata-only measurement: overlay externalProcessKey with a constant
// synthetic PID probe; never run against an actual Codex process or home.
func TestHookIdentityMeasure(t *testing.T) {
	if os.Getenv("ASB_HOOK_IDENTITY_MEASURE") != "1" {
		t.Skip("opt-in synthetic process-probe overlay required")
	}
	for _, dirs := range []int{35, 365} {
		for _, device := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%v", dirs, device), func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("CODEX_HOME", home)
				for n := 0; n < dirs; n++ {
					date := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
					if err := os.MkdirAll(filepath.Join(home, "sessions", date.Format("2006/01/02")), 0700); err != nil {
						t.Fatal(err)
					}
				}
				f := newHookIdentityFixture(t, device)
				b := f.binding.Load()
				b.mu.Lock()
				id := b.identity
				b.mu.Unlock()
				data, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id.SessionID, "cwd": id.CWD}})
				if err := os.WriteFile(filepath.Join(home, "sessions", "2025", "01", "01", "rollout-synthetic-"+id.SessionID+".jsonl"), append(data, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
				adapter := codex.NewAdapter()
				f.observe = func(ctx context.Context, thread string) (broker.AttachRequest, error) {
					return adapter.ObserveExternal(ctx, thread, id.PID)
				}
				f.calls.Store(0)
				start := time.Now()
				for n := 0; n < 500; n++ {
					event := "PreToolUse"
					if n%2 != 0 {
						event = "PostToolUse"
					}
					if f.hook(t, event, id.SessionID, id.SessionID).IsError {
						t.Fatal("synthetic hook rejected")
					}
				}
				t.Logf("dirs=%d device=%v hooks=500 identify=%d ObserveExternal=%d wall=%s", dirs, device, f.calls.Load(), f.calls.Load(), time.Since(start))
			})
		}
	}
}
