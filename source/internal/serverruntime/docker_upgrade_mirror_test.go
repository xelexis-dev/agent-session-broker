//go:build !windows

package serverruntime

// Opt-in local Docker verification. Only u2-* resources and synthetic data.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"io"
	"net"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func u2Docker(t *testing.T, input []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("docker", args...)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	matrixRequire(t, err == nil, "disposable Docker operation")
	return out
}
func TestDockerUpgradeMirror(t *testing.T) {
	if os.Getenv("ASB_U2_013") == "" || os.Getenv("ASB_U2_015") == "" {
		t.Skip("opt-in disposable Docker upgrade matrix")
	}
	helper := phase6BuildRelay(t)
	for _, gap := range []time.Duration{0, 60 * time.Second} {
		if !t.Run(fmt.Sprintf("gap-%ds", int(gap.Seconds())), func(t *testing.T) { u2Case(t, helper, gap) }) {
			return
		}
	}
}
func u2Case(t *testing.T, helper string, gap time.Duration) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	name, volume := "u2-broker-"+suffix, "u2-data-"+suffix
	u2Docker(t, nil, "volume", "create", volume)
	// Cleanup runs on test failure as well. No resource name comes from production.
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		if exec.Command("docker", "volume", "rm", volume).Run() != nil {
			t.Error("disposable volume cleanup failed")
		}
	})
	dir := t.TempDir()
	parent, _, observerToken, _, before := matrixSeed(t, dir, "synthetic-seed")
	tar, err := exec.Command("tar", "-cf", "-", "-C", dir, ".").Output()
	matrixRequire(t, err == nil, "synthetic seed archive")
	u2Docker(t, tar, "run", "--rm", "-i", "-v", volume+":/data", "alpine:3.23", "sh", "-c", "tar -xf - -C /data && chown -R 65532:65532 /data")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	matrixRequire(t, err == nil, "disposable port")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	start := func(version string) time.Time {
		u2Docker(t, nil, "run", "-d", "--name", name, "--restart", "unless-stopped", "-p", fmt.Sprintf("127.0.0.1:%d:8787", port), "-v", volume+":/data", "ghcr.io/xelexis-dev/agent-session-broker-deploy:"+version)
		matrixWait(t, "container healthy", func() bool {
			return strings.TrimSpace(string(u2Docker(t, nil, "inspect", "--format", "{{.State.Health.Status}}", name))) == "healthy"
		})
		return time.Now()
	}
	stop := func() { u2Docker(t, nil, "stop", name); u2Docker(t, nil, "rm", name) }
	snapshot := func() []byte {
		u2Docker(t, nil, "pause", name)
		b := u2Docker(t, nil, "run", "--rm", "-v", volume+":/data:ro", "alpine:3.23", "tar", "-cf", "-", "-C", "/data", ".")
		u2Docker(t, nil, "unpause", name)
		return b
	}
	start("0.14.0")
	target, _ := url.Parse(origin)
	browser := &deviceRuntime{t: t, h: httputil.NewSingleHostReverseProxy(target), project: "test", password: "synthetic-matrix-password"}
	browser.login(browser.password)
	type live struct {
		c        *matrixClient
		name, id string
		pid      int
		binary   string
		queue    string
	}
	var clients []live
	for i, binary := range []string{os.Getenv("ASB_U2_013"), os.Getenv("ASB_U2_015")} {
		version := []string{"0.13.0", "0.15.0"}[i]
		out, err := exec.Command(binary, "--version").Output()
		matrixRequire(t, err == nil && strings.Contains(string(out), version), "client version")
		home, auth, cwd := t.TempDir(), t.TempDir(), t.TempDir()
		rollout := filepath.Join(home, ".codex", "sessions", "2026", "10", "15")
		os.MkdirAll(rollout, 0700)
		meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": matrixSID, "cwd": cwd}})
		os.WriteFile(filepath.Join(rollout, "rollout-u2-"+matrixSID+".jsonl"), append(meta, '\n'), 0600)
		queue := filepath.Join(home, "native-queue.json")
		c := u2Start(t, binary, helper, origin+"/mcp?project=test", parent, auth, home, cwd, queue)
		var status struct{ Status, UserCode string }
		matrixWait(t, "manual pending", func() bool {
			return c.tool(t.Context(), "auth_status", map[string]any{}, &status) && status.Status == "pending"
		})
		nickname := fmt.Sprintf("worker-%d", i)
		w := browser.browser("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {status.UserCode}, "nickname": {nickname}})
		matrixRequire(t, w.Code == 303, "initial approval")
		matrixRequire(t, c.tool(t.Context(), "ingress_hook", map[string]string{"session_id": matrixSID, "hook_event_name": "PreToolUse"}, nil), "initial ingress hook")
		var own broker.Session
		matrixWait(t, "initial ready", func() bool {
			return c.tool(t.Context(), "get_session", map[string]string{"name": nickname}, &own) && own.AutomaticDeliveryReady
		})
		clients = append(clients, live{c, nickname, own.RegistrationID, own.PID, version, queue})
	}
	backup := snapshot()
	observer := matrixHTTPPeer(t, origin, observerToken)
	observer.close()
	stop()
	downAt := time.Now()
	// Exercise an actual existing stdio client during the outage, not auth_status.
	var gapResult []broker.Session
	ok := clients[0].c.tool(t.Context(), "list_sessions", map[string]any{}, &gapResult)
	t.Logf("gap tool completed success=%t client-alive=%t", ok, syscall.Kill(clients[0].pid, 0) == nil)
	if remaining := gap - time.Since(downAt); remaining > 0 {
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-t.Context().Done():
			t.Fatal("gap canceled")
		}
	}
	healthy := start("0.15.0")
	// Separate authenticated HTTP observer: no calls to existing clients until ready.
	observer = matrixHTTPPeer(t, origin, observerToken)
	for _, c := range clients {
		var own broker.Session
		matrixWait(t, "unstimulated upgrade recovery", func() bool {
			return observer.tool(t.Context(), "get_session", map[string]string{"name": c.name}, &own) && own.Status == "alive" && own.AutomaticDeliveryReady
		})
		matrixRequire(t, own.RegistrationID == c.id && own.Name == c.name && own.PID == c.pid && syscall.Kill(c.pid, 0) == nil, "same registration/name/live PID")
		t.Logf("PASS upgrade client=%s gap=%ds healthy-to-ready=%.3fs same-id/name/pid", c.binary, int(gap.Seconds()), time.Since(healthy).Seconds())
		matrixRequire(t, c.c.tool(t.Context(), "get_session", map[string]string{"name": c.name}, &own), "first post-upgrade tool")
		var auth struct{ Status string }
		matrixRequire(t, c.c.tool(t.Context(), "auth_status", map[string]any{}, &auth) && auth.Status != "pending" && auth.Status != "rejected", "no reapproval")
	}
	messages := func(label string) {
		for i, c := range clients {
			to := clients[1-i]
			body := "u2-" + label + "-" + c.binary
			var sent broker.SendResult
			matrixRequire(t, c.c.tool(t.Context(), "send_message", broker.Message{From: c.name, To: to.name, Body: body}, &sent) && sent.Accepted, "bidirectional send")
			var hook map[string]any
			matrixWait(t, "bidirectional ingress delivery", func() bool {
				queued, _ := os.ReadFile(to.queue)
				if bytes.Contains(queued, []byte(body)) {
					return true
				}
				if !to.c.tool(t.Context(), "ingress_hook", map[string]string{"session_id": matrixSID, "hook_event_name": "PostToolUse"}, &hook) {
					return false
				}
				raw, _ := json.Marshal(hook)
				return strings.Contains(string(raw), body)
			})
		}
		t.Log("PASS bidirectional actual ingress delivery " + label)
	}
	messages("upgrade")
	observer.close()
	migrated := snapshot()
	stop()
	copyDir := t.TempDir()
	cmd := exec.Command("tar", "-xf", "-", "-C", copyDir)
	cmd.Stdin = bytes.NewReader(migrated)
	matrixRequire(t, cmd.Run() == nil, "synthetic migrated snapshot extract")
	matrixVerifyUpgrade(t, copyDir, before)
	// Restore the complete frozen pre-upgrade volume, never edit a v4 payload.
	u2Docker(t, backup, "run", "--rm", "-i", "-v", volume+":/data", "alpine:3.23", "sh", "-c", "find /data -mindepth 1 -delete && tar -xf - -C /data")
	healthy = start("0.14.0")
	observer = matrixHTTPPeer(t, origin, observerToken)
	defer observer.close()
	for _, c := range clients {
		var own broker.Session
		matrixWait(t, "rollback recovery", func() bool {
			return observer.tool(t.Context(), "get_session", map[string]string{"name": c.name}, &own) && own.AutomaticDeliveryReady
		})
		matrixRequire(t, own.RegistrationID == c.id && own.Name == c.name && syscall.Kill(c.pid, 0) == nil, "rollback same identity/client alive")
		matrixRequire(t, c.c.tool(t.Context(), "get_session", map[string]string{"name": c.name}, &own), "rollback first tool")
		t.Logf("PASS rollback client=%s healthy-to-ready=%.3fs", c.binary, time.Since(healthy).Seconds())
	}
	messages("rollback")
	for _, c := range clients {
		c.c.close()
	}
	stop()
}

func u2Start(t *testing.T, binary, helper, endpoint, token, auth, home, cwd, queue string) *matrixClient {
	t.Helper()
	cmd := exec.Command(helper, binary, "--broker-url", endpoint, "--token", token, "--auth-dir", auth)
	cmd.Dir = cwd
	cmd.Stderr = io.Discard
	cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "PATH=" + filepath.Dir(helper) + ":" + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "CODEX_HOME=" + filepath.Join(home, ".codex"), "CODEX_THREAD_ID=" + matrixSID, "PHASE6_QUEUE=" + queue}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "u2-scripted", Version: "test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
	matrixRequire(t, err == nil, "stdio initialize")
	c := &matrixClient{session}
	t.Cleanup(c.close)
	return c
}
