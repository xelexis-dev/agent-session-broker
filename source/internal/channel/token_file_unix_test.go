//go:build darwin || linux

package channel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO with no writer, given directly or through a symlink, must be refused promptly, not block startup.
func TestTokenFileRefusesFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link} {
		done := make(chan error, 1)
		go func() {
			_, err := parseClientOptions([]string{"--broker-url", testURL, "--token-file", path})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("%s: %v", filepath.Base(path), err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: token file open blocked", filepath.Base(path))
		}
	}
}

// A symlink to a private regular file follows the target's checks.
func TestTokenFileFollowsSymlinkToPrivateFile(t *testing.T) {
	target := writeTokenFile(t, "asb_p_linked\n", 0o600)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	opts, err := parseClientOptions([]string{"--broker-url", testURL, "--token-file", link})
	if err != nil || opts.token != "asb_p_linked" {
		t.Fatalf("%+v %v", opts, err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseClientOptions([]string{"--broker-url", testURL, "--token-file", link}); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("shared target accepted through symlink: %v", err)
	}
}
