package channel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverruntime"
)

func TestProjectTokenManagementSnapshotForwardsToProtectedBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Setup("synthetic-tool-snapshot-password"); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateProject("tools"); err != nil {
		t.Fatal(err)
	}
	if err = store.SetInstanceHTTPPolicy("admin", true); err != nil {
		t.Fatal(err)
	}
	if err = store.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	ownerToken, err := store.IssueProjectTokenWithOptionsFor("admin", "tools", "manager-parent", controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
	if err != nil {
		t.Fatal(err)
	}
	workerScopes := []string{access.SessionRegister, access.SessionRegisterAuto, access.SessionRead, access.MessageReceive}
	workerToken, err := store.IssueProjectTokenWithOptionsFor("admin", "tools", "worker-parent", controlpanel.ProjectTokenOptions{Scopes: workerScopes})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	serverCtx, stopServer := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- serverruntime.Run(serverCtx, []string{"--listen", address, "--memory-dir", root}, "test", io.Discard)
	}()
	t.Cleanup(func() {
		stopServer()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(7 * time.Second):
			t.Error("temporary runtime did not stop")
		}
	})
	origin := "http://" + address
	health := &http.Client{Timeout: time.Second}
	waitFor(t, "temporary management runtime", func() bool {
		res, err := health.Get(origin + "/healthz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
	endpoint := origin + "/mcp?project=tools"
	newHarness := func(token string, n int) (*mcp.ClientSession, broker.Session) {
		t.Helper()
		state, err := clientstate.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		identity := broker.AttachRequest{Harness: "codex", SessionID: fmt.Sprintf("11111111-1111-4111-8111-%012d", n), PID: n, CWD: t.TempDir()}
		opts := durableOptions{enabled: true, tokenMode: true, state: state, scope: brokerScope(endpoint, "tools"), project: "tools", heartbeat: 50 * time.Millisecond, timeout: time.Second, backoff: func(int) time.Duration { return 10 * time.Millisecond }, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return identity, nil }}
		opts.device = &deviceAuthOptions{authorizer: newDeviceAuthClient(endpoint, token, nil), clock: realPollClock(), newUpstream: func(credential string) Upstream {
			u := newTestBrokerUpstream(ctx, endpoint)
			u.setBearerToken(credential)
			return u
		}}
		local, shutdown := newIngressServerWithOptions(ctx, unavailableUpstream{}, snapshotTools(), func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return identity, nil }, func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
			return func(context.Context, broker.Message) error { return nil }, nil, nil
		}, opts)
		t.Cleanup(shutdown)
		st, ct := mcp.NewInMemoryTransports()
		ss, err := local.Connect(ctx, st, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ss.Close() })
		harness, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-project-token-harness", Version: "test"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { harness.Close() })
		listed, err := harness.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"suspend_session", "unsuspend_session"} {
			if !hasTool(listed.Tools, name) {
				t.Fatalf("project-token client lacks %s", name)
			}
		}
		var session broker.Session
		if err := call(ctx, harness, "attach_session", map[string]any{}, &session); err != nil {
			t.Fatal("explicit auto attach failed", err)
		}
		if session.RegistrationID == "" {
			t.Fatal("auto registration missing")
		}
		return harness, session
	}
	owner, _ := newHarness(ownerToken, 811)
	_, target := newHarness(ownerToken, 812)
	worker, _ := newHarness(workerToken, 813)
	for _, name := range []string{"suspend_session", "unsuspend_session"} {
		result, err := worker.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]string{"name": target.RegistrationID}})
		if err != nil || !result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, access.ErrInsufficientScope.Error()) {
			t.Fatalf("worker call did not preserve server scope denial: %s", name)
		}
	}
	var current broker.Session
	if err := call(ctx, owner, "get_session", map[string]string{"name": target.RegistrationID}, &current); err != nil || current.Suspended {
		t.Fatal("denied worker mutated target")
	}
	for _, step := range []struct {
		tool   string
		paused bool
	}{{"suspend_session", true}, {"unsuspend_session", false}} {
		if err := call(ctx, owner, step.tool, map[string]string{"name": target.RegistrationID}, nil); err != nil {
			t.Fatal("Owner forwarded call failed", step.tool, err)
		}
		current = broker.Session{} // false/omitted JSON fields must not retain the prior suspended snapshot.
		if err := call(ctx, owner, "get_session", map[string]string{"name": target.RegistrationID}, &current); err != nil || current.Suspended != step.paused || current.LastManagement == nil || current.LastManagement.Kind != strings.TrimSuffix(step.tool, "_session")+"ed" {
			t.Fatal("forwarded call did not reach protected management backend", step.tool)
		}
	}
}
