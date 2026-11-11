package controlpanel

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLanguagePreviousBinaryReadsCopiedData(t *testing.T) {
	binary := os.Getenv("ASB_LANGUAGE_PREVIOUS_SERVER")
	if binary == "" {
		t.Skip("opt-in previous public release binary")
	}
	version, err := exec.Command(binary, "--version").Output()
	if err != nil || (strings.TrimSpace(string(version)) != "asb-server 1.0.3" && strings.TrimSpace(string(version)) != "asb-server 1.0.4") {
		t.Fatal("unexpected previous release binary")
	}
	original := t.TempDir()
	s, err := OpenSQLite(filepath.Join(original, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateLanguage("admin", "ko"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	copied := t.TempDir()
	if err = os.CopyFS(copied, os.DirFS(original)); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--memory-dir", copied, "--listen", "127.0.0.1:0")
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			marker := "control panel listening on "
			if i := strings.Index(line, marker); i >= 0 {
				ready <- line[i+len(marker):]
				return
			}
		}
		ready <- ""
	}()
	var base string
	select {
	case base = <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("old server startup timed out")
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatal("old server did not start on copied preference data")
	}
	response, err := http.Get(strings.TrimRight(base, "/") + "/login")
	if err != nil {
		t.Fatal("old server request failed")
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("old server login unavailable")
	}
	_ = cmd.Process.Signal(os.Interrupt)
	if err = cmd.Wait(); err != nil {
		t.Fatal("old server shutdown failed")
	}
	stopped = true
	s, err = OpenSQLite(filepath.Join(copied, "control"))
	if err != nil {
		t.Fatal("cannot reopen old-server copy")
	}
	defer s.Close()
	if s.UserLanguage("admin") != "ko" {
		t.Fatal("old server discarded preference row")
	}
	t.Log("previous release started on copied data and preserved preference rows")
}
