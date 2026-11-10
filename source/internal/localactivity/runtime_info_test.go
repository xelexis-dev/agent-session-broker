package localactivity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexRuntimeInfoSelectedFieldsAndIntegrity(t *testing.T) {
	f := newFixture(t, "codex")
	line := `{"type":"turn_context","payload":{"model":"gpt-6.1-sol","effort":"medium","content":"ignored synthetic content"}}` + "\n"
	f.write(f.meta(sessionA) + line)
	got := f.sample()
	if got.Model != "gpt-6.1-sol" || got.Effort != "medium" {
		t.Fatal("selected runtime metadata absent")
	}
	// Activity can be unknown while independently verified model metadata exists.
	if got.State != Unknown {
		t.Fatal("metadata changed activity")
	}
	f.append(`{"type":"turn_context","payload":{"model":"gpt-6.1-sol","effort":"xhigh"}}` + "\n")
	got = f.sample()
	if got.Effort != "xhigh" {
		t.Fatal("latest effort absent")
	}
	f.write(f.meta(sessionA) + strings.Replace(line, "gpt-6.1-sol", "bad<model>", 1))
	if got = f.sample(); got.Model != "" || got.Effort != "" {
		t.Fatal("rewrite leaked old metadata")
	}
}
func TestClaudeRuntimeInfoBoundTranscript(t *testing.T) {
	f := newFixture(t, "claude")
	f.config.Format = ""
	f.config.RuntimeInfo = true
	f.path = filepath.Join(f.config.Home, "projects", claudeSlugChars.ReplaceAllString(f.config.CWD, "-"), sessionA+".jsonl")
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		t.Fatal(err)
	}
	f.observer = New(f.config)
	entry := func(id, model, effort string) string {
		raw, _ := json.Marshal(map[string]any{"type": "assistant", "sessionId": id, "cwd": f.config.CWD, "effort": effort, "message": map[string]any{"model": model, "content": "ignored synthetic content"}})
		return string(raw) + "\n"
	}
	f.write(entry(sessionA, "claude-opus-5-5", "xhigh"))
	got := f.sample()
	if got.Model != "claude-opus-5-5" || got.Effort != "xhigh" || got.State != Unknown || got.Reason != "unsupported" {
		t.Fatal("bound transcript runtime metadata failed")
	}
	got = f.sample()
	if got.Model != "claude-opus-5-5" {
		t.Fatal("unchanged transcript lost model")
	}
	f.append(entry(sessionA, "claude-sonnet-5-5", "high"))
	if got = f.sample(); got.Model != "claude-sonnet-5-5" || got.Effort != "high" {
		t.Fatal("append offset did not update")
	}
	f.append(entry(sessionB, "foreign", "low"))
	if got = f.sample(); got.Model != "" || got.Reason != "unsupported" {
		t.Fatal("foreign entry accepted")
	}
}
func TestRuntimeInfoBindingAndLabels(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + `{"type":"turn_context","payload":{"model":"bad/model","effort":"` + strings.Repeat("x", 17) + `"}}` + "\n")
	if got := f.sample(); got.Model != "" || got.Effort != "" {
		t.Fatal("unsafe labels accepted")
	}
	binding := f.config.Binding
	binding.Generation++
	if got := f.observer.Observe(context.Background(), binding); got.Model != "" || got.Reason != "identity-mismatch" {
		t.Fatal("changed binding accepted")
	}
}

func TestMalformedRuntimeLabelsDoNotChangeCodexActivity(t *testing.T) {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + f.event("task_started", "turn-a") + `{"type":"turn_context","payload":{"model":17,"effort":false}}` + "\n")
	got := f.sample()
	if got.State != Working || got.Model != "" || got.Effort != "" {
		t.Fatal("malformed optional model metadata changed activity")
	}
}
