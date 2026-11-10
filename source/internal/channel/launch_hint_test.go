package channel

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const wantLauncherHint = "a launcher process stays between the harness and this client (for example npx); see xbroker_docs topic=setup"

func TestLaunchHintAuthStatusAncestry(t *testing.T) {
	for _, kind := range []string{"intermediate", "direct", "unknown", "failed"} {
		t.Run(kind, func(t *testing.T) {
			// Inherited npm launcher state must not affect the ancestry decision.
			t.Setenv("npm_command", "exec")
			t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
			parent := duplicateParent(t)
			switch kind {
			case "direct":
				parent.ancestors = func() ([]int, error) { return []int{61530, 1}, nil }
			case "unknown":
				parent.classify = func(int) (string, error) { return "", nil }
			case "failed":
				parent.ancestors = func() ([]int, error) { return []int{123, 61530}, errors.New("synthetic unavailable") }
			}
			cs := launchHintClient(t, []string{"--broker-url", "http://127.0.0.1:1/mcp?project=test", "--token", "asb_p_synthetic", "--state-dir", t.TempDir()}, parent)
			r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "auth_status", Arguments: struct{}{}})
			if err != nil {
				t.Fatal("auth_status transport failed", err)
			}
			raw, err := json.Marshal(r.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			_, diagPresent := fields["parentDiagnostic"]
			if diagPresent != (kind == "unknown" || kind == "failed") {
				t.Fatal("failure diagnostic presence mismatch")
			}
			value, present := fields["launchHint"]
			want := kind == "intermediate" && runtime.GOOS != "windows"
			if present != want || present && value != wantLauncherHint {
				t.Fatalf("hint presence=%t, want %t", present, want)
			}
			// Older typed consumers ignore additive fields while keeping known fields.
			var old struct {
				Status     string `json:"status"`
				Registered bool   `json:"registered"`
			}
			if err = json.Unmarshal(raw, &old); err != nil {
				t.Fatal("old consumer rejected additive status", err)
			}
			if !r.IsError && old.Status == "" {
				t.Fatal("known status field lost")
			}
			catalog, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range catalog.Tools {
				if tool.Name != "auth_status" {
					continue
				}
				schemaRaw, _ := json.Marshal(tool.OutputSchema)
				var schema struct {
					Properties map[string]any `json:"properties"`
					Required   []string       `json:"required"`
				}
				if err := json.Unmarshal(schemaRaw, &schema); err != nil {
					t.Fatal(err)
				}
				if _, ok := schema.Properties["launchHint"]; !ok {
					t.Fatal("existing hint schema lost")
				}
				if _, ok := schema.Properties["parentDiagnostic"]; !ok {
					t.Fatal("new field absent from negotiated schema")
				}
				for _, name := range schema.Required {
					if name == "parentDiagnostic" || name == "launchHint" {
						t.Fatal("additive hint became required")
					}
				}
			}
		})
	}
}

func launchHintClient(t *testing.T, args []string, parent clientParentSource) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- runClientTransport(ctx, st, args, parent) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("client did not stop")
		}
	})
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "old-harness", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestLaunchHintPlatforms(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, tc := range []struct {
			name    string
			parents []int
			pid     int
			want    bool
		}{
			{"intermediate", []int{123, 456}, 456, true},
			{"direct", []int{456, 1}, 456, false},
			{"unknown", nil, 0, false},
			{"missing-harness", []int{123}, 0, false},
			{"missing-parent", []int{0, 456}, 456, false},
		} {
			t.Run(platform+"/"+tc.name, func(t *testing.T) {
				got := launcherHint(platform, tc.parents, tc.pid)
				want := tc.want && platform != "windows"
				if (got != "") != want || got != "" && got != wantLauncherHint {
					t.Fatalf("unexpected hint presence for %s", platform)
				}
			})
		}
	}
}
