package delivery

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSplitExecutables(t *testing.T) {
	for _, name := range []string{"asb-client", "asb-server"} {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), name)
			if runtime.GOOS == "windows" {
				binary += ".exe"
			}
			cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version=1.2.3", "-o", binary, "../../cmd/"+name)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			for _, flag := range []string{"--version", "--help"} {
				out, err := exec.Command(binary, flag).CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v\n%s", flag, err, out)
				}
				if !strings.Contains(string(out), name) {
					t.Fatalf("%s output: %s", flag, out)
				}
				if flag == "--version" && string(out) != name+" 1.2.3\n" {
					t.Fatalf("version: %q", out)
				}
			}
			if out, err := exec.Command(binary, "--not-a-flag").CombinedOutput(); err == nil {
				t.Fatalf("invalid flags accepted: %s", out)
			}
			if out, err := exec.Command(binary, "unexpected-positional").CombinedOutput(); err == nil {
				t.Fatalf("positional argument accepted: %s", out)
			}
		})
	}
}

func TestServerDependencyBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "../../cmd/asb-server").CombinedOutput()
	if err != nil {
		t.Fatalf("dependencies: %v\n%s", err, out)
	}
	for _, forbidden := range []string{"channel", "claude", "codex", "process"} {
		if strings.Contains(string(out), "/internal/"+forbidden+"\n") {
			t.Errorf("server imports %s", forbidden)
		}
	}
}

func TestAdapterDependencyBoundary(t *testing.T) {
	for _, target := range []string{"../claude", "../codex"} {
		out, err := exec.Command("go", "list", "-deps", target).CombinedOutput()
		if err != nil {
			t.Fatalf("dependencies: %v\n%s", err, out)
		}
		for _, forbidden := range []string{"broker", "groups", "mcpserver", "controlpanel", "serverruntime"} {
			if strings.Contains(string(out), "/internal/"+forbidden+"\n") {
				t.Errorf("%s imports %s implementation", target, forbidden)
			}
		}
	}
}

func TestClientDependencyBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "../../cmd/asb-client").CombinedOutput()
	if err != nil {
		t.Fatalf("dependencies: %v\n%s", err, out)
	}
	for _, forbidden := range []string{"broker", "groups", "mcpserver", "memory", "controlpanel", "serverruntime"} {
		if strings.Contains(string(out), "/internal/"+forbidden+"\n") {
			t.Errorf("client imports %s implementation", forbidden)
		}
	}
}
