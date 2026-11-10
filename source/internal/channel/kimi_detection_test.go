//go:build darwin || linux

package channel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKimiNodeEntryDetection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	for _, tc := range []struct{ entry, title, want string }{{"@moonshot-ai/kimi-code/dist/main.mjs", "kimi-code", "kimi"}, {"other/main.mjs", "other-title", ""}, {"@moonshot-ai/kimi-code/dist/main.mjs", "", "kimi"}} {
		t.Run(tc.entry, func(t *testing.T) {
			entry := filepath.Join(t.TempDir(), tc.entry)
			if err := os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
				t.Fatal(err)
			}
			titleAssignment := ""
			if tc.title != "" {
				titleAssignment = "process.title=" + jsonPath(tc.title) + ";"
			}
			script := "import fs from 'node:fs'; " + titleAssignment + "fs.writeFileSync(" + jsonPath(entry+".ready") + ",'ready'); setInterval(()=>{},1000);"
			if err := os.WriteFile(entry, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(node, entry)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			waitFor(t, "node startup", func() bool { _, e := os.Stat(entry + ".ready"); return e == nil })
			got, err := processHarness(cmd.Process.Pid)
			if err != nil || got != tc.want {
				t.Fatalf("harness=%q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}
func TestUnsupportedParentGuidance(t *testing.T) {
	_, _, err := nearestAgent([]int{1}, func(int) (string, error) { return "", nil })
	if err == nil || !strings.HasPrefix(err.Error(), "unsupported-harness:") {
		t.Fatalf("wrong unsupported parent guidance: %v", err)
	}
}

func jsonPath(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestKimiEntrySymlinkAndBareNode(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "@moonshot-ai/kimi-code/dist/main.mjs")
	_ = os.MkdirAll(filepath.Dir(entry), 0700)
	_ = os.WriteFile(entry, nil, 0600)
	link := filepath.Join(root, "kimi")
	if e := os.Symlink(entry, link); e != nil {
		t.Fatal(e)
	}
	if h := harnessFromProcess("/usr/bin/node", link, ""); h != "kimi" {
		t.Fatal("symlink entry not detected", h)
	}
	for _, script := range []string{"", "-e", "--eval", "npm-cli.js"} {
		if h := harnessFromProcess("/usr/bin/node", script, ""); h != "" {
			t.Fatal("plain Node accepted")
		}
	}
	calls := 0
	_, _, err := nearestAgent([]int{10, 20}, func(int) (string, error) { calls++; return "", os.ErrPermission })
	if err != errUnsupportedHarness || calls != 1 {
		t.Fatal("continued after unreadable ancestor", err, calls)
	}
}
