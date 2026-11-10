//go:build darwin || linux

package codexcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestLauncherTypedAllowlist(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := &LauncherClient{client: newClient(client), thread: "U", valid: func() bool { return true }}
	defer c.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := bufio.NewReader(server)
		for _, method := range []string{"thread/loaded/list", "mcpServerStatus/list", "mcpServer/tool/call"} {
			_, raw, err := readClientFrame(r)
			if err != nil {
				t.Error(err)
				return
			}
			var q struct {
				ID     any            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			json.Unmarshal(raw, &q)
			if q.Method != method {
				t.Errorf("unexpected method %s", q.Method)
				return
			}
			var result any
			switch method {
			case "thread/loaded/list":
				result = map[string]any{"data": []string{"V", "U"}}
			case "mcpServerStatus/list":
				if q.Params["threadId"] != "U" || q.Params["serverName"] != "x-broker" {
					t.Error("wrong inventory target")
				}
				result = map[string]any{"data": []any{map[string]any{"name": "x-broker", "runtimeStatus": "connected"}}}
			case "mcpServer/tool/call":
				if q.Params["threadId"] != "U" || q.Params["server"] != "x-broker" || q.Params["tool"] != "auth_status" || q.Params["_meta"] != nil || len(q.Params["arguments"].(map[string]any)) != 0 {
					t.Error("arbitrary launcher dispatch")
				}
				result = map[string]any{"content": []any{}}
			}
			if reply(server, q.ID, result) != nil {
				return
			}
		}
	}()
	if ok, err := c.Loaded(t.Context()); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := c.Connected(t.Context()); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := c.AuthStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-done
	if _, err := c.client.Call(t.Context(), "thread/loaded/list", nil); !errors.Is(err, ErrMethod) {
		t.Fatal("steer allowlist was widened")
	}
}
func TestLauncherPinChecks(t *testing.T) {
	for _, which := range []string{"inode-first", "inode-replaced", "peer", "key", "mode", "home"} {
		t.Run(which, func(t *testing.T) {
			f := fakeDaemon(t)
			info, _ := os.Stat(f.target)
			inode := socketInode(info)
			key := "generation-1"
			if which == "inode-first" {
				inode++
			}
			l, err := BindLauncher(f.home, f.target, inode, "U", func(int) (string, error) { return key, nil })
			if err != nil {
				t.Fatal(err)
			}
			if which == "mode" {
				os.Chmod(f.target, 0644)
			}
			if which == "home" {
				f.responseHome.Store(t.TempDir())
			}
			c, err := l.Dial(t.Context())
			if which == "inode-first" || which == "mode" || which == "home" {
				if !errors.Is(err, ErrUnverifiedHost) {
					if c != nil {
						c.Close()
					}
					t.Fatal("bad endpoint accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
			<-f.finished
			switch which {
			case "key":
				key = "generation-2"
			case "peer":
				l.host.peer = func(*net.UnixConn) (int, uint32, error) { return os.Getpid() + 1, uint32(os.Getuid()), nil }
			case "inode-replaced":
				f.listener.SetUnlinkOnClose(false)
				if os.Rename(f.target, f.target+"old") != nil {
					t.Fatal("rename")
				}
				replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: f.target, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer replacement.Close()
				os.Chmod(f.target, 0600)
			}
			if c, err := l.Dial(context.Background()); !errors.Is(err, ErrUnverifiedHost) {
				if c != nil {
					c.Close()
				}
				t.Fatal("changed daemon accepted")
			}
		})
	}
}

func TestLauncherAliasPinsCanonicalTarget(t *testing.T) {
	f := fakeDaemon(t)
	alias := filepath.Join(f.home, "control-alias.sock")
	if err := os.Symlink(f.target, alias); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.target)
	if err != nil {
		t.Fatal(err)
	}
	l, err := BindLauncher(f.home, alias, socketInode(info), "U", func(int) (string, error) { return "same-process", nil })
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	<-f.finished
	other := fakeDaemon(t)
	if os.Remove(alias) != nil || os.Symlink(other.target, alias) != nil {
		t.Fatal("replace owned alias")
	}
	if l.valid() {
		t.Fatal("retargeted alias kept original pin")
	}
	if c, err := l.Dial(t.Context()); !errors.Is(err, ErrUnverifiedHost) {
		if c != nil {
			c.Close()
		}
		t.Fatal("retargeted alias accepted")
	}
}
