//go:build darwin || linux

package serverruntime

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

func TestCodexDaemonResumeE2E(t *testing.T) {
	binary := os.Getenv("ASB_CODEX_DAEMON_RESUME_BINARY")
	if binary == "" {
		t.Skip("opt-in ASB_CODEX_DAEMON_RESUME_BINARY")
	}
	data := t.TempDir()
	control, e := controlpanel.OpenSQLite(filepath.Join(data, "control"))
	matrixRequire(t, e == nil, "control open")
	matrixRequire(t, control.Setup("synthetic-claim-fixture-password") == nil && control.CreateProject("test") == nil, "control setup")
	matrixRequire(t, control.SetInstanceHTTPPolicy("admin", true) == nil && control.SetScopeIssuanceReady("admin", true) == nil, "loopback policy")
	parent, e := control.IssueProjectTokenWithOptionsFor("admin", "test", "claim fixture", controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto), UnlimitedAutoBindings: true})
	matrixRequire(t, e == nil, "synthetic project admission")
	peer, e := control.IssueTokenFor("admin", "test", "peer", "admin")
	matrixRequire(t, e == nil, "synthetic peer")
	matrixRequire(t, control.Close() == nil, "control close")
	handler, closeRuntime, e := newManagedHandler(data, nil, "0.17.3")
	matrixRequire(t, e == nil, "runtime")
	defer closeRuntime()
	var observer *matrixClient
	var requestMu sync.Mutex
	requests := []map[string]any{}
	credentialRequests := 0
	type holderView struct {
		PID        int    `json:"holderPID"`
		Generation uint64 `json:"generation"`
		lease      string
	}
	holders := map[string]holderView{}
	leasePIDs := map[string]int{}
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && (r.URL.Path == "/oauth/session_registration" || r.URL.Path == "/oauth/device_authorization") {
			requestMu.Lock()
			credentialRequests++
			requestMu.Unlock()
		}
		if r.Method != "POST" || r.URL.Path != "/mcp" {
			handler.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var q struct {
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					SessionID string `json:"sessionId"`
					ChannelID string `json:"channelId"`
					Holder    struct {
						PID int `json:"pid"`
					} `json:"holder"`
				} `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &q)
		tool := q.Params.Name
		if tool != "resume_session" && tool != "register_session" && tool != "report_ingress_failure" {
			handler.ServeHTTP(w, r)
			return
		}
		writer := &claimControlWriter{ResponseWriter: w}
		handler.ServeHTTP(writer, r)
		reply, decoded := claimReply(writer.body.Bytes())
		ok := decoded && reply.Code == "" && (reply.Session.ID != "" || reply.OK)
		requestMu.Lock()
		defer requestMu.Unlock()
		thread := q.Params.Arguments.SessionID
		pid := q.Params.Arguments.Holder.PID
		lease := q.Params.Arguments.ChannelID
		if tool == "report_ingress_failure" {
			pid = leasePIDs[lease]
		}
		if ok && reply.Session.ID != "" {
			thread = reply.Session.ID
			current := holders[thread]
			if reply.Session.Generation >= current.Generation {
				holders[thread] = holderView{PID: pid, Generation: reply.Session.Generation, lease: lease}
			}
			leasePIDs[lease] = pid
		} else if reply.HolderChallenge != nil {
			holders[thread] = holderView{PID: reply.HolderChallenge.Holder.PID, Generation: reply.HolderChallenge.Generation, lease: holders[thread].lease}
		}
		if tool == "report_ingress_failure" && ok {
			current := holders[thread]
			if current.lease == lease {
				current.PID = 0
				holders[thread] = current
			}
		}
		code := reply.Code
		if code != "" && code != "registration-in-use" {
			code = "refused"
		}
		requests = append(requests, map[string]any{"at": float64(time.Now().UnixNano()) / 1e9, "tool": tool, "thread": thread, "holderPID": pid, "generation": reply.Session.Generation, "ok": ok, "code": code})
	}))
	mux.HandleFunc("/probe/holder", func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		defer requestMu.Unlock()
		_ = json.NewEncoder(w).Encode(holders[r.URL.Query().Get("thread")])
	})
	mux.HandleFunc("/probe/events", func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		defer requestMu.Unlock()
		_ = json.NewEncoder(w).Encode(requests)
	})
	mux.HandleFunc("/probe/roster", func(w http.ResponseWriter, r *http.Request) {
		var rows []map[string]any
		if !observer.tool(r.Context(), "list_sessions", map[string]any{}, &rows) {
			w.WriteHeader(500)
			return
		}
		safe := []map[string]any{}
		for _, row := range rows {
			s := map[string]any{}
			for _, k := range []string{"id", "name", "status", "automaticDeliveryReady", "registrationId", "generation", "revision", "errorCode"} {
				s[k] = row[k]
			}
			safe = append(safe, s)
		}
		_ = json.NewEncoder(w).Encode(safe)
	})
	mux.HandleFunc("/probe/counters", func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		defer requestMu.Unlock()
		registers := 0
		for _, event := range requests {
			if event["tool"] == "register_session" {
				registers++
			}
		}
		json.NewEncoder(w).Encode(map[string]int{"credentialRequests": credentialRequests, "registrations": registers})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	observer = matrixHTTPPeer(t, server.URL, peer)
	launcher, e := filepath.Abs("../channel/testdata/codex-resume.sh")
	matrixRequire(t, e == nil, "launcher path")
	payload, _ := json.Marshal(map[string]string{"binary": binary, "endpoint": server.URL + "/mcp?project=test", "origin": server.URL, "token": parent, "launcher": launcher})
	command := exec.CommandContext(t.Context(), "python3", "testdata/codex_daemon_resume.py")
	command.Stdin = bytes.NewReader(payload)
	// Driver emits only fixed labels/booleans/timestamps. Never echo child diagnostics/config.
	var out bytes.Buffer
	command.Stdout = &out
	command.Stderr = io.Discard
	err := command.Run()
	t.Log(out.String())
	if err != nil {
		t.Fatal("real Codex daemon resume E2E failed; inspect sanitized evidence")
	}
}
