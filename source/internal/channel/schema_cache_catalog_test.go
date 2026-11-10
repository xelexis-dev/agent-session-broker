package channel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/mcpserver"
)

// Baselines come from the uncached builder; intentional public prose changes
// rebaseline only after a cached/uncached byte comparison.
var uncachedSchemaCatalogs = map[string]string{
	"durable-channel/full":   "12e267597f1af40f6297e94dcece83e80aa723264df3b250b5fe4657ce710e49",
	"durable-channel/worker": "22d332b9d0ea9952f360f4fb03dd653887cc03ad050d57034c91342f6eb112bc",
	"codex-ingress/full":     "e2cc7641be62b8ea38d38c8ae4c661311fb88f5c0dc6a49ed756ba6ff202ed7b",
	"codex-ingress/worker":   "7e681d19a1b71df9bc0e09f82e538e4c5a9e129b1d814ad015dcdf193f2d0c5c",
	"claude-ingress/full":    "99a3cc7b710ba60eaf08a7105a227e0ed12b8ca90dffaf7ac2ecd977b2f2d1d5",
	"claude-ingress/worker":  "16f92d700d62af556349a1251b97e577e553d86e3a70439615b463a023e156bb",
	"kimi-ingress/full":      "99a3cc7b710ba60eaf08a7105a227e0ed12b8ca90dffaf7ac2ecd977b2f2d1d5",
	"kimi-ingress/worker":    "16f92d700d62af556349a1251b97e577e553d86e3a70439615b463a023e156bb",
}

func TestSchemaCacheHarnessCatalogs(t *testing.T) {
	backend := broker.NewRemoteService()
	defer backend.Close()
	central := connectRejectionTest(t, t.Context(), mcpserver.New(backend, noMemory{}))
	listed, e := central.ListTools(t.Context(), nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"durable-channel", "codex-ingress", "claude-ingress", "kimi-ingress"} {
		for _, profile := range []string{"full", "worker"} {
			t.Run(path+"/"+profile, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.WithValue(t.Context(), toolProfileKey{}, profile))
				defer cancel()
				up := &profileUpstream{}
				id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/synthetic"}
				identify := func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return id, nil }
				st, ct := mcp.NewInMemoryTransports()
				done := make(chan struct{})
				switch path {
				case "durable-channel":
					id.Harness = "claude"
					go func() { defer close(done); ServeDurableChannel(ctx, st, up, listed.Tools, id, durableOptions{}) }()
				default:
					opts := durableOptions{harness: strings.TrimSuffix(path, "-ingress")}
					if opts.harness == "kimi" {
						opts.kimi = newKimiClient(123, "/synthetic")
					}
					server, shutdown := newIngressServerWithOptions(ctx, up, listed.Tools, identify, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
						return func(context.Context, broker.Message) error { return nil }, nil, nil
					}, opts)
					ss, e := server.Connect(ctx, st, nil)
					if e != nil {
						t.Fatal(e)
					}
					defer ss.Close()
					defer shutdown()
					close(done)
				}
				defer func() { cancel(); <-done }()
				client, e := mcp.NewClient(&mcp.Implementation{Name: "schema-catalog", Version: "1"}, nil).Connect(ctx, ct, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer client.Close()
				tools, e := client.ListTools(ctx, nil)
				if e != nil {
					t.Fatal(e)
				}
				raw, e := json.Marshal(tools)
				if e != nil {
					t.Fatal(e)
				}
				key := path + "/" + profile
				hash := fmt.Sprintf("%x", sha256.Sum256(raw))
				if dir := os.Getenv("ASB_SCHEMA_CATALOG_CAPTURE"); dir != "" {
					if os.WriteFile(filepath.Join(dir, strings.ReplaceAll(key, "/", "-")+".json"), raw, 0600) != nil {
						t.Fatal("capture")
					}
					t.Logf("CATALOG %s %s", key, hash)
					return
				}
				if hash != uncachedSchemaCatalogs[key] {
					t.Fatalf("%s tools/list bytes changed: %s", key, hash)
				}
			})
		}
	}
}
