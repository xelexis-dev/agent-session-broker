package channel

import "testing"

func TestHarnessFromExecutable(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/home/test/.local/share/claude/versions/2.1.304", "claude"},
		{"/home/test/.local/share/claude/versions/2.1.304-dev+build.4", "claude"},
		{"/usr/local/bin/claude", "claude"},
		{"/usr/local/bin/codex", "codex"},
		{"/home/test/.local/share/codex/versions/1.2.3", ""},
		{"/home/test/.local/share/other/versions/2.1.304", ""},
		{"/usr/local/bin/2.1.304", ""},
		{"/home/test/.local/share/claude/versions/node", ""},
		{"/home/test/.local/share/claude/versions/v2.1.304", ""},
		{"/home/test/.local/share/claude/versions/2.1", ""},
		{"/home/test/.local/share/claude/versions/2.1.304_dev", ""},
		{"/home/test/.local/share/claude/releases/2.1.304", ""},
		{"/home/test/.local/share/not-claude/versions/2.1.304", ""},
		{"/home/test/.local/share/claude/versions/2.1.304/worker", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := harnessFromExecutable(tc.path); got != tc.want {
				t.Fatalf("harness=%q want=%q", got, tc.want)
			}
		})
	}
}
func TestNearestAgentFindsVersionedBackgroundParent(t *testing.T) {
	paths := map[int]string{51946: "/home/test/.local/share/claude/versions/2.1.304", 51941: "/home/test/.local/share/claude/versions/2.1.304", 51894: "/usr/local/bin/claude"}
	harness, pid, err := nearestAgent([]int{51946, 51941, 51894}, func(pid int) (string, error) { return harnessFromExecutable(paths[pid]), nil })
	if err != nil || harness != "claude" || pid != 51946 {
		t.Fatalf("nearest background parent=%s/%d err=%v", harness, pid, err)
	}
}
