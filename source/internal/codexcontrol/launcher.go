//go:build darwin || linux

package codexcontrol

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Launcher pins one explicit daemon endpoint, rather than a Codex MCP ancestor.
type Launcher struct {
	mu     sync.Mutex
	host   Host
	inode  uint64
	thread string
	pid    int
	key    string
}

func BindLauncher(home, path string, inode uint64, thread string, key func(int) (string, error)) (*Launcher, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(path) || inode == 0 || thread == "" || key == nil {
		return nil, ErrUnverifiedHost
	}
	info, err := os.Lstat(home)
	if err != nil || (info.Mode()&os.ModeSymlink != 0 && !owned(info)) {
		return nil, ErrUnverifiedHost
	}
	return &Launcher{host: Host{home: home, path: path, processKey: key, peer: peerIdentity}, inode: inode, thread: thread}, nil
}
func (l *Launcher) Dial(ctx context.Context) (*LauncherClient, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, err := l.host.dial(ctx, l.inode, func(pid int) bool {
		key, e := l.host.processKey(pid)
		if e != nil || key == "" {
			return false
		}
		if l.pid == 0 {
			l.pid, l.key = pid, key
		}
		return pid == l.pid && key == l.key
	})
	if err != nil {
		return nil, err
	}
	key, e := l.host.processKey(l.pid)
	if e != nil || key != l.key {
		c.Close()
		return nil, ErrUnverifiedHost
	}
	return &LauncherClient{client: c, thread: l.thread, valid: l.valid}, nil
}

// Typed methods fix the launcher allowlist, server, tool and thread at binding.
type LauncherClient struct {
	client *Client
	thread string
	valid  func() bool
}

func (c *LauncherClient) Close() error { return c.client.Close() }
func (c *LauncherClient) Loaded(ctx context.Context) (bool, error) {
	if !c.valid() {
		return false, ErrUnverifiedHost
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	cursor := ""
	seen := map[string]bool{}
	for {
		args := map[string]any{"limit": 100}
		if cursor != "" {
			args["cursor"] = cursor
		}
		raw, err := c.client.exchange(ctx, "thread/loaded/list", args, false)
		if err != nil {
			return false, err
		}
		var page struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &page) != nil || page.Data == nil {
			return false, ErrProtocol
		}
		for _, id := range page.Data {
			if id == c.thread {
				return true, nil
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return false, nil
		}
		cursor = *page.NextCursor
		if seen[cursor] {
			return false, ErrProtocol
		}
		seen[cursor] = true
	}
}
func (c *LauncherClient) Connected(ctx context.Context) (bool, error) {
	if !c.valid() {
		return false, ErrUnverifiedHost
	}
	raw, err := c.client.exchange(ctx, "mcpServerStatus/list", map[string]any{"threadId": c.thread, "serverName": "x-broker", "detail": "toolsAndAuthOnly"}, false)
	if err != nil {
		return false, err
	}
	var page struct {
		Data []struct{ Name, RuntimeStatus string } `json:"data"`
	}
	if json.Unmarshal(raw, &page) != nil || page.Data == nil {
		return false, ErrProtocol
	}
	for _, server := range page.Data {
		if server.Name == "x-broker" {
			return server.RuntimeStatus == "connected", nil
		}
	}
	return false, nil
}
func (c *LauncherClient) AuthStatus(ctx context.Context) (json.RawMessage, error) {
	if !c.valid() {
		return nil, ErrUnverifiedHost
	}
	return c.client.exchange(ctx, "mcpServer/tool/call", map[string]any{"threadId": c.thread, "server": "x-broker", "tool": "auth_status", "arguments": map[string]any{}}, false)
}

func (l *Launcher) valid() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, info, err := socketTarget(l.host.path)
	if err != nil || socketInode(info) != l.inode || l.pid == 0 {
		return false
	}
	key, err := l.host.processKey(l.pid)
	return err == nil && key == l.key
}
