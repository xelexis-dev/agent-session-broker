package channel

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

func TestClientInitializePrunesIdleStateWithoutRemovingLockedOrRecent(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "discover"
		if legacy {
			name = "initialize"
		}
		t.Run(name, func(t *testing.T) { checkClientStartupPrune(t, legacy) })
	}
}

func checkClientStartupPrune(t *testing.T, legacy bool) {
	dir := t.TempDir()
	store, err := clientstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, name := range []string{"idle", "locked", "recent"} {
		key := clientstate.SessionKey("synthetic", "codex", name, "synthetic-client")
		path := filepath.Join(dir, "sessions", key)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(path, "conn.json")
		if err := os.WriteFile(file, []byte("synthetic content need not be decoded"), 0600); err != nil {
			t.Fatal(err)
		}
		if name != "recent" {
			old := time.Now().Add(-31 * 24 * time.Hour)
			if err := os.Chtimes(file, old, old); err != nil {
				t.Fatal(err)
			}
		}
		if name == "locked" {
			unlock, err := store.LockSession(key)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
		}
		paths[name] = path
	}
	_ = newDurableFixtureWithPolicy(t, newSwitchableUpstream(t), dir, false, func(opts *durableOptions, _ *func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error)) {
		if legacy {
			opts.configure = func(server *mcp.Server) {
				server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
						if method == "server/discover" {
							return nil, &jsonrpc.Error{Code: -32601, Message: "synthetic legacy server"}
						}
						return next(ctx, method, req)
					}
				})
			}
		}
	})
	waitFor(t, "MCP initialize must prune idle unlocked session", func() bool { _, err := os.Lstat(paths["idle"]); return os.IsNotExist(err) })
	for _, name := range []string{"locked", "recent"} {
		if _, err := os.Stat(paths[name]); err != nil {
			t.Fatalf("%s state removed", name)
		}
	}
}
