package controlpanel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

// Public original bytes only, against copies of synthetic SQLite data.
func TestAutoTokenUnlimitedReal016Downgrade(t *testing.T) {
	binary := os.Getenv("ASB_UNLIMITED_OLD_SERVER")
	if binary == "" {
		t.Skip("opt-in original 0.16.0 binary")
	}
	data, err := os.ReadFile(binary)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != "904e3df3602578b6c214207096a84be978228cf2db28ed778d99a0f16b719629" {
		t.Fatal("public 0.16.0 darwin-arm64 Release digest mismatch")
	}
	version, err := exec.CommandContext(t.Context(), binary, "--version").Output()
	if err != nil || string(bytes.TrimSpace(version)) != "asb-server 0.16.0" {
		t.Fatal("old binary version mismatch")
	}
	for _, tc := range []struct {
		name      string
		unlimited bool
		cap, days int
		loads     bool
	}{{"finite", false, 64, 90, true}, {"unlimited", true, 0, 0, false}, {"outside-old-bounds", false, 65, 91, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := directStore(t)
			_, err := s.IssueProjectTokenWithOptionsFor("admin", "demo", tc.name, ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto), MaxAutoBindings: tc.cap, AutoLifetimeDays: tc.days, UnlimitedAutoBindings: tc.unlimited, UnlimitedAutoLifetime: tc.unlimited})
			if err != nil {
				t.Fatal("synthetic issuance failed")
			}
			if err = s.Close(); err != nil {
				t.Fatal("synthetic close failed")
			}
			copied := t.TempDir()
			// The store root is control/. Copy only this stopped synthetic fixture.
			control := filepath.Join(copied, "control")
			if err = os.Mkdir(control, 0700); err != nil {
				t.Fatal("copy directory failed")
			}
			entries, err := os.ReadDir(s.root.Name())
			if err != nil {
				t.Fatal("copy source failed")
			}
			for _, entry := range entries {
				if !entry.Type().IsRegular() {
					continue
				}
				b, err := os.ReadFile(filepath.Join(s.root.Name(), entry.Name()))
				if err != nil || os.WriteFile(filepath.Join(control, entry.Name()), b, 0600) != nil {
					t.Fatal("synthetic copy failed")
				}
			}
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal("fixture port failed")
			}
			address := l.Addr().String()
			l.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "--listen", address, "--memory-dir", copied)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err = cmd.Start(); err != nil {
				t.Fatal("old binary start failed")
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			joined := false
			defer func() {
				if !joined {
					cmd.Process.Kill()
					<-done
				}
			}()
			client := &http.Client{Timeout: 500 * time.Millisecond}
			timer := time.NewTimer(10 * time.Second)
			defer timer.Stop()
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case err := <-done:
					joined = true
					if tc.loads || err == nil {
						t.Fatal("unexpected old-server exit")
					}
					t.Log("old 0.16.0 rejects synthetic state before serving")
					return
				case <-timer.C:
					t.Fatal("old server neither served nor rejected within watchdog")
				case <-ticker.C:
					response, err := client.Get("http://" + address + "/healthz")
					if err != nil {
						continue
					}
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if !tc.loads {
						t.Fatal("old server accepted unsupported admission limits")
					}
					if response.StatusCode != 200 {
						continue
					}
					response, err = client.Get("http://" + address + "/login")
					if err != nil {
						t.Fatal("old login failed")
					}
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.StatusCode != 200 {
						t.Fatal("old login not 200")
					}
					t.Log("old 0.16.0 loads finite synthetic state; health/login 200")
					cmd.Process.Signal(os.Interrupt)
					<-done
					joined = true
					return
				}
			}
		})
	}
}
