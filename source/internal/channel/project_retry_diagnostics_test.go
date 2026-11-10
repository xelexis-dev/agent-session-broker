package channel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestProjectUnavailableReconnectDiagnosticsAndRecovery(t *testing.T) {
	for _, code := range []string{"project_disabled", "project_deleting"} {
		t.Run(code, func(t *testing.T) {
			var disabled atomic.Bool
			disabled.Store(true)
			remote := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
				return mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.17.3"}, nil)
			}, &mcp.StreamableHTTPOptions{Stateless: true})
			central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if disabled.Load() {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "30")
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprintf(w, `{"error":%q}`, code)
					return
				}
				remote.ServeHTTP(w, r)
			}))
			defer central.Close()
			up := newTestBrokerUpstream(t.Context(), central.URL)
			defer up.Close()
			up.setBearerToken("synthetic-project-credential")
			up.projectState, _ = newProjectTracker(nil, "scope")
			up.projectState.learn(up.projectSource, "", strings.Repeat("A", 26), true)
			state, err := clientstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const sessionID = "11111111-1111-4111-8111-111111111111"
			binding := &ingressBinding{mu: &sync.Mutex{}, identity: protocol.AttachRequest{Harness: "codex", SessionID: sessionID, PID: 123, CWD: "/synthetic"}, durable: &durableRegistration{key: clientstate.RegistrationKey("scope", "codex", sessionID), registrationID: strings.Repeat("B", 26), secret: "synthetic-proof", revision: 7, clientID: "synthetic-client"}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts := (durableOptions{state: state, backoff: resumeFixtureBackoff}).withDefaults()
			waits := 0
			opts.resumeClock = devicePollClock{sleep: func(_ context.Context, delay time.Duration) error {
				waits++
				if want := time.Second << uint(waits-1); delay != want {
					t.Errorf("retry delay = %v, want %v", delay, want)
				}
				out := authVersions(t.Context(), up, authStatus{Status: authAuthorized, Diagnostics: opts.diagnostics(binding)})
				want := strings.ReplaceAll(code, "_", "-")
				if out.Diagnostics == nil || out.Diagnostics.Unavailable || out.Diagnostics.LastFailureCode != want || out.Diagnostics.LastFailureClass != want {
					t.Errorf("reconnect refusal lost from auth_status diagnostics: want %s", want)
				}
				if !strings.Contains(out.Message, code) || out.Status != authAuthorized || binding.resumeStopReason != "" || upstreamRejected(up) {
					t.Error("temporary refusal lost from auth_status or became terminal/credential rejection")
				}
				if waits == 2 {
					cancel()
				}
				return ctx.Err()
			}}
			if _, ok := resumeDurable(ctx, up, binding, opts, ""); ok || waits != 2 {
				t.Fatal("temporary reconnect did not keep retrying")
			}
			if binding.durable.revision != 7 || binding.durable.secret != "synthetic-proof" {
				t.Fatal("temporary refusal changed saved proof")
			}
			disabled.Store(false)
			if _, err := up.session(t.Context()); err != nil {
				t.Fatal("re-enable did not recover")
			}
			out := authVersions(t.Context(), up, authStatus{Status: authAuthorized, Message: "ready"})
			if out.Message != "ready" {
				t.Fatal("recovered auth_status retained temporary refusal")
			}
		})
	}
}

func TestProjectUnavailableDiagnosticsRequireKnownSupport(t *testing.T) {
	for _, body := range []string{`{"error":"project_disabled"}`, `{"error":"project_deleting"}`, `{"error":"private-arbitrary-text"}`} {
		tracker, err := newProjectTracker(nil, "scope")
		if err != nil {
			t.Fatal(err)
		}
		if got := diagnosticErrorClass(tracker.refusal("unknown", http.StatusForbidden, []byte(body)), false); got != "other" {
			t.Fatal("untrusted refusal exposed as project diagnostic")
		}
	}
	if got := diagnosticErrorClass(fmt.Errorf("project_disabled: private-arbitrary-text"), false); got != "other" {
		t.Fatal("free-form error became a project diagnostic")
	}
	if got := diagnosticErrorClass(&projectRefusalError{code: "project_deleted", terminal: true}, false); got != "project-deleted" {
		t.Fatal("terminal diagnostic changed")
	}
}
