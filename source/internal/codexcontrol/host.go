//go:build darwin || linux

package codexcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var ErrUnverifiedHost = errors.New("unverified-host")

// Host captures the binding's already-observed nearest Codex ancestor. key is
// the existing process-key reader (channel's kimiProcessKey), not a new probe.
type Host struct {
	home, path string
	pid        int
	key        string
	processKey func(int) (string, error)
	peer       func(*net.UnixConn) (int, uint32, error)
}

func Bind(pid int, key func(int) (string, error)) (*Host, error) {
	if pid <= 0 || key == nil {
		return nil, ErrUnverifiedHost
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return nil, ErrUnverifiedHost
		}
		home = filepath.Join(user, ".codex")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return nil, ErrUnverifiedHost
	}
	info, err := os.Lstat(home)
	if err != nil || (info.Mode()&os.ModeSymlink != 0 && !owned(info)) {
		return nil, ErrUnverifiedHost
	}
	h := &Host{home: home, path: filepath.Join(home, "app-server-control", "app-server-control.sock"), pid: pid, processKey: key, peer: peerIdentity}
	h.key, err = key(pid)
	if err != nil || h.key == "" {
		return nil, ErrUnverifiedHost
	}
	return h, nil
}
func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
func socketTarget(path string) (string, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", nil, ErrUnverifiedHost
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if !owned(info) {
			return "", nil, ErrUnverifiedHost
		}
		link, err := os.Readlink(path)
		if err != nil {
			return "", nil, ErrUnverifiedHost
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(path), link)
		}
		path = filepath.Clean(link)
		info, err = os.Lstat(path)
		if err != nil {
			return "", nil, ErrUnverifiedHost
		}
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || !owned(parent) || parent.Mode().Perm()&0022 != 0 || !owned(info) || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return "", nil, ErrUnverifiedHost
	}
	return path, info, nil
}
func (h *Host) sameProcess() bool { key, err := h.processKey(h.pid); return err == nil && key == h.key }
func (h *Host) sameHome(home string) bool {
	if home == "" {
		return false
	}
	expected, err := filepath.EvalSymlinks(h.home)
	if err != nil {
		return false
	}
	actual, err := filepath.EvalSymlinks(home)
	if err != nil {
		return false
	}
	a, err := os.Stat(expected)
	if err != nil {
		return false
	}
	b, err := os.Stat(actual)
	return err == nil && a.IsDir() && b.IsDir() && os.SameFile(a, b)
}
func (h *Host) Dial(ctx context.Context) (*Client, error) {
	if !h.sameProcess() {
		return nil, ErrUnverifiedHost
	}
	return h.dial(ctx, 0, func(pid int) bool { return pid == h.pid && h.sameProcess() })
}
func (h *Host) dial(ctx context.Context, inode uint64, verifyPeer func(int) bool) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	path, before, err := socketTarget(h.path)
	if err != nil || (inode != 0 && socketInode(before) != inode) {
		return nil, ErrUnverifiedHost
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, ErrUnverifiedHost
	}
	c := newClient(conn)
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	peerPID, peerUID, err := h.peer(conn.(*net.UnixConn))
	_, after, recheck := socketTarget(h.path)
	if err != nil || peerUID != uint32(os.Getuid()) || !verifyPeer(peerPID) || recheck != nil || !os.SameFile(before, after) {
		return nil, ErrUnverifiedHost
	}
	deadline, _ := ctx.Deadline()
	if conn.SetDeadline(deadline) != nil {
		return nil, ErrUnverifiedHost
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); close(joined) })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	if err = c.ws.upgrade(); err != nil {
		return nil, exchangeError(ctx, err)
	}
	raw, err := c.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "x-broker", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
	if err != nil {
		return nil, err
	}
	var response struct {
		CodexHome string `json:"codexHome"`
	}
	if json.Unmarshal(raw, &response) != nil || !h.sameHome(response.CodexHome) {
		return nil, ErrUnverifiedHost
	}
	if err = c.initialized(ctx); err != nil {
		return nil, err
	}
	ok = true
	return c, nil
}

func socketInode(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Ino)
}
