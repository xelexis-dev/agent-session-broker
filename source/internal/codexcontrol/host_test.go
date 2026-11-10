//go:build darwin || linux

package codexcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type daemonFixture struct {
	home, path, target string
	host               *Host
	listener           *net.UnixListener
	calls              atomic.Int32
	initialized        atomic.Int32
	experimental       atomic.Bool
	responseHome       atomic.Value
	bytesRead          atomic.Int32
	finished           chan struct{}
	stallUpgrade       atomic.Bool
}

func fakeDaemon(t *testing.T) *daemonFixture {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "cc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := &daemonFixture{finished: make(chan struct{}, 1), home: filepath.Join(root, "home"), target: filepath.Join(root, "target", "s")}
	for _, dir := range []string{filepath.Join(f.home, "app-server-control"), filepath.Dir(f.target)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.path = filepath.Join(f.home, "app-server-control", "app-server-control.sock")
	f.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: f.target, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if os.Chmod(f.target, 0600) != nil || os.Symlink(f.target, f.path) != nil {
		t.Fatal("socket fixture")
	}
	f.responseHome.Store(f.home)
	t.Setenv("CODEX_HOME", f.home)
	f.host, err = Bind(os.Getpid(), func(int) (string, error) { return "process-start-key", nil })
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := f.listener.AcceptUnix()
			if err != nil {
				return
			}
			f.serve(conn)
		}
	}()
	t.Cleanup(func() { f.listener.Close(); <-done })
	return f
}
func (f *daemonFixture) serve(conn *net.UnixConn) {
	defer conn.Close()
	defer func() { f.finished <- struct{}{} }()
	r := bufio.NewReader(countedReader{Reader: conn, count: &f.bytesRead})
	request, err := http.ReadRequest(r)
	if err != nil {
		return
	}
	if f.stallUpgrade.Load() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		return
	}
	if writeAll(conn, validUpgrade(request.Header.Get("Sec-WebSocket-Key"))) != nil {
		return
	}
	for {
		op, raw, err := readClientFrame(r)
		if err != nil || op == 8 {
			return
		}
		var q struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Capabilities struct {
					Experimental bool `json:"experimentalApi"`
				} `json:"capabilities"`
			} `json:"params"`
		}
		if json.Unmarshal(raw, &q) != nil {
			return
		}
		if q.Method == "initialized" {
			f.initialized.Add(1)
			continue
		}
		f.calls.Add(1)
		result := map[string]any{"data": []any{}}
		if q.Method == "initialize" {
			f.experimental.Store(q.Params.Capabilities.Experimental)
			result = map[string]any{"codexHome": f.responseHome.Load().(string)}
		}
		if reply(conn, q.ID, result) != nil {
			return
		}
	}
}
func TestVerifiedSocketAndPhysicalHome(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-path", true: "alias-directory"}[alias], func(t *testing.T) {
			f := fakeDaemon(t)
			if alias {
				link := filepath.Join(filepath.Dir(f.home), "home-alias")
				if os.Symlink(f.home, link) != nil {
					t.Fatal("alias")
				}
				f.responseHome.Store(link)
			}
			c, err := f.host.Dial(t.Context())
			if err != nil {
				t.Fatal("verified daemon refused", err)
			}
			if _, err := c.Call(t.Context(), "thread/turns/list", map[string]any{"threadId": "U", "limit": 1}); err != nil {
				t.Fatal(err)
			}
			c.Close()
			if f.calls.Load() != 2 || f.initialized.Load() != 1 {
				t.Fatal("initialize/initialized/turns exchange missing")
			}
		})
	}
}
func TestVerifierRefusals(t *testing.T) {
	for _, kind := range []string{"0644", "parent-write", "peer-pid", "peer-uid", "key-before", "key-after", "home-missing", "home-mismatch", "symlink-chain", "regular-file"} {
		t.Run(kind, func(t *testing.T) {
			f := fakeDaemon(t)
			switch kind {
			case "0644":
				_ = os.Chmod(f.target, 0644)
			case "parent-write":
				_ = os.Chmod(filepath.Dir(f.target), 0770)
			case "peer-pid":
				f.host.pid++
			case "peer-uid":
				f.host.peer = func(*net.UnixConn) (int, uint32, error) { return os.Getpid(), uint32(os.Getuid()) ^ 1, nil }
			case "key-before":
				f.host.processKey = func(int) (string, error) { return "changed", nil }
			case "key-after":
				n := 0
				f.host.processKey = func(int) (string, error) {
					n++
					if n == 1 {
						return f.host.key, nil
					}
					return "reused-pid", nil
				}
			case "home-missing":
				f.responseHome.Store("")
			case "home-mismatch":
				f.responseHome.Store(filepath.Dir(f.home))
			case "symlink-chain":
				_ = os.Remove(f.path)
				link := filepath.Join(filepath.Dir(f.target), "link")
				_ = os.Symlink(f.target, link)
				_ = os.Symlink(link, f.path)
			case "regular-file":
				_ = os.Remove(f.path)
				_ = os.WriteFile(f.path, []byte("not a socket"), 0600)
			}
			c, err := f.host.Dial(t.Context())
			if c != nil {
				c.Close()
			}
			switch kind {
			case "peer-pid", "peer-uid", "key-after", "home-missing", "home-mismatch":
				<-f.finished
			}
			if (kind == "peer-pid" || kind == "peer-uid" || kind == "key-after") && f.bytesRead.Load() != 0 {
				t.Fatal("wrote bytes to unverified peer")
			}
			if !errors.Is(err, ErrUnverifiedHost) {
				t.Fatal("unsafe daemon accepted", kind, err)
			}
			expected := int32(0)
			if kind == "home-missing" || kind == "home-mismatch" {
				expected = 1
			}
			if f.calls.Load() != expected || f.initialized.Load() != 0 {
				t.Fatal("unverified host received method beyond verification")
			}
		})
	}
}

type foreignOwner struct{ os.FileInfo }

func (f foreignOwner) Sys() any { s := *f.FileInfo.Sys().(*syscall.Stat_t); s.Uid ^= 1; return &s }
func TestForeignFileOwner(t *testing.T) {
	f := fakeDaemon(t)
	info, err := os.Lstat(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if owned(foreignOwner{info}) {
		t.Fatal("foreign UID accepted")
	}
}
func TestHomeResolutionMatchesRolloutDefault(t *testing.T) {
	f := fakeDaemon(t)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", filepath.Dir(f.home))
	if err := os.Rename(f.home, filepath.Join(filepath.Dir(f.home), ".codex")); err != nil {
		t.Fatal(err)
	}
	h, err := Bind(os.Getpid(), func(int) (string, error) { return "key", nil })
	if err != nil || h.home != filepath.Join(filepath.Dir(f.home), ".codex") {
		t.Fatal("rollout default changed")
	}
	if _, err := Bind(0, nil); !errors.Is(err, ErrUnverifiedHost) {
		t.Fatal("unbound parent accepted")
	}
}
func TestHandshakeDeadline(t *testing.T) {
	f := fakeDaemon(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if c, err := f.host.Dial(ctx); err == nil {
		c.Close()
		t.Fatal("cancelled connect succeeded")
	}
}

type countedReader struct {
	io.Reader
	count *atomic.Int32
}

func (r countedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.count.Add(int32(n))
	return n, err
}
func TestHostUpgradeDeadline(t *testing.T) {
	f := fakeDaemon(t)
	f.stallUpgrade.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	if _, err := f.host.Dial(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("upgrade deadline missing", err)
	}
}

func TestRecoveryQueueNegotiatesExperimentalAPI(t *testing.T) {
	f := fakeDaemon(t)
	c, err := f.host.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !f.experimental.Load() {
		t.Fatal("queue recovery RPC refused: experimentalApi not negotiated")
	}
	if _, err = c.Call(t.Context(), "thread/queue/list", map[string]any{"threadId": "U"}); err != nil {
		t.Fatal(err)
	}
}
