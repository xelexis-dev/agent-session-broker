//go:build darwin || linux

package serverruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

// Opt-in actual Codex daemon + product client + managed loopback broker.
func TestCodexSteerE2E(t *testing.T) {
	binary := os.Getenv("ASB_CODEX_STEER_BINARY")
	if binary == "" {
		t.Skip("opt-in ASB_CODEX_STEER_BINARY")
	}
	data := t.TempDir()
	panel, err := controlpanel.OpenSQLite(filepath.Join(data, "control"))
	matrixRequire(t, err == nil, "control")
	matrixRequire(t, panel.Setup("synthetic-steer-password") == nil && panel.CreateProject("test") == nil, "setup")
	matrixRequire(t, panel.SetInstanceHTTPPolicy("admin", true) == nil && panel.SetScopeIssuanceReady("admin", true) == nil, "policy")
	token, err := panel.IssueProjectTokenWithOptionsFor("admin", "test", "steer", controlpanel.ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto), UnlimitedAutoBindings: true})
	matrixRequire(t, err == nil, "admission")
	credential, err := panel.IssueTokenFor("admin", "test", "peer", "admin")
	matrixRequire(t, err == nil, "peer")
	matrixRequire(t, panel.Close() == nil, "close")
	var handler http.Handler
	var runtimeMu sync.RWMutex
	var restart func() error
	if legacy := os.Getenv("ASB_CODEX_STEER_LEGACY_SERVER"); legacy != "" {
		handler = legacySteerHandler(t, data, legacy)
	} else {
		managed, stop, err := newManagedHandler(data, nil, "0.17.3")
		matrixRequire(t, err == nil, "runtime")
		t.Cleanup(func() { stop() })
		handler = managed
		restart = func() error {
			if err := stop(); err != nil {
				return err
			}
			next, nextStop, err := newManagedHandler(data, nil, "0.17.3")
			if err != nil {
				return err
			}
			runtimeMu.Lock()
			handler = next
			stop = nextStop
			runtimeMu.Unlock()
			return nil
		}
	}
	var observer *matrixClient
	var ackMu sync.Mutex
	type ackView struct {
		Registration string  `json:"registration"`
		Through      uint64  `json:"through"`
		At           float64 `json:"at"`
	}
	acks := map[string]ackView{}
	serveRuntime := func(w http.ResponseWriter, r *http.Request) {
		runtimeMu.RLock()
		h := handler
		runtimeMu.RUnlock()
		h.ServeHTTP(w, r)
	}
	var recoveryMu sync.Mutex
	var recoveryTarget string
	var recoveryWaiting bool
	recoveryHolds := 0
	admissions, freshAdmissions, registerCalls := 0, 0, 0
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/oauth/session_registration" {
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(raw))
			form, _ := url.ParseQuery(string(raw))
			proof := form.Get("registration_id")
			recoveryMu.Lock()
			admissions++
			if proof == "" {
				freshAdmissions++
			}
			hold := proof != "" && proof == recoveryTarget && !recoveryWaiting
			if hold {
				recoveryWaiting = true
				recoveryHolds++
			}
			recoveryMu.Unlock()
			if hold {
				select {
				case <-r.Context().Done():
					recoveryMu.Lock()
					if recoveryTarget == proof {
						recoveryTarget = ""
					}
					recoveryMu.Unlock()
				case <-time.After(15 * time.Second):
				}
				return
			}
		}
		if r.Method != "POST" || r.URL.Path != "/mcp" {
			serveRuntime(w, r)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var request struct {
			Params struct {
				Name      string              `json:"name"`
				Arguments broker.RenewRequest `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &request)
		recoveryMu.Lock()
		if request.Params.Name == "register_session" {
			registerCalls++
		}
		hold := recoveryTarget != "" && request.Params.Arguments.RegistrationID == recoveryTarget
		recoveryMu.Unlock()
		if (request.Params.Name == "wait_for_message" || request.Params.Name == "renew_lease") && os.Getenv("ASB_CODEX_STEER_CASES") == "x1d" {
			recorder := httptest.NewRecorder()
			serveRuntime(recorder, r)
			recoveryMu.Lock()
			hold = recoveryTarget != "" && request.Params.Arguments.RegistrationID == recoveryTarget
			recoveryMu.Unlock()
			if hold {
				select {
				case <-r.Context().Done():
				case <-time.After(15 * time.Second):
				}
				return
			}
			for k, values := range recorder.Header() {
				for _, v := range values {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		if hold && request.Params.Name == "renew_lease" {
			select {
			case <-r.Context().Done():
			case <-time.After(15 * time.Second):
			}
			return
		}

		if request.Params.Name != "renew_lease" || request.Params.Arguments.AckThrough == 0 {
			serveRuntime(w, r)
			return
		}
		at := float64(time.Now().UnixNano()) / 1e9
		capture := &claimControlWriter{ResponseWriter: w}
		serveRuntime(capture, r)
		result, ok := claimReply(capture.body.Bytes())
		if ok && result.Code == "" && result.Session.ID != "" {
			ackMu.Lock()
			args := request.Params.Arguments
			if args.AckThrough > acks[args.RegistrationID].Through {
				acks[args.RegistrationID] = ackView{args.RegistrationID, args.AckThrough, at}
			}
			ackMu.Unlock()
		}
	}))
	mux.HandleFunc("/probe/acks", func(w http.ResponseWriter, r *http.Request) {
		ackMu.Lock()
		defer ackMu.Unlock()
		json.NewEncoder(w).Encode(acks)
	})
	mux.HandleFunc("/probe/roster", func(w http.ResponseWriter, r *http.Request) {
		var rows []map[string]any
		if !observer.tool(r.Context(), "list_sessions", map[string]any{}, &rows) {
			w.WriteHeader(500)
			return
		}
		safe := []map[string]any{}
		for _, row := range rows {
			v := map[string]any{}
			for _, k := range []string{"id", "name", "status", "automaticDeliveryReady", "registrationId", "generation", "errorCode", "pendingMessages"} {
				v[k] = row[k]
			}
			safe = append(safe, v)
		}
		json.NewEncoder(w).Encode(safe)
	})
	mux.HandleFunc("/probe/send", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ To, Body string }
		if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in) != nil {
			w.WriteHeader(400)
			return
		}
		var out map[string]any
		var target broker.Session
		found := observer.tool(r.Context(), "get_session", map[string]any{"name": in.To}, &target)
		ok := found && observer.tool(r.Context(), "send_message", map[string]any{"from": "peer", "to": in.To, "body": in.Body, "expectRegistrationId": target.RegistrationID}, &out)
		json.NewEncoder(w).Encode(map[string]any{"ok": ok, "accepted": out["accepted"]})
	})
	mux.HandleFunc("/probe/manage", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Tool      string
			Arguments map[string]any
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || (in.Tool != "stop_session" && in.Tool != "retire_session") {
			w.WriteHeader(400)
			return
		}
		var out map[string]any
		ok := observer.tool(r.Context(), in.Tool, in.Arguments, &out)
		json.NewEncoder(w).Encode(map[string]any{"ok": ok, "result": out})
	})
	mux.HandleFunc("/probe/panel", func(w http.ResponseWriter, r *http.Request) {
		runtimeMu.RLock()
		h := handler
		runtimeMu.RUnlock()
		browser := &deviceRuntime{t: t, h: h}
		browser.login("synthetic-steer-password")
		page := browser.browser("GET", "/projects/sessions?project=test", nil)
		json.NewEncoder(w).Encode(map[string]any{"ok": page.Code == 200, "containsTarget": strings.Contains(page.Body.String(), r.URL.Query().Get("name"))})
	})
	mux.HandleFunc("/probe/restart", func(w http.ResponseWriter, r *http.Request) {
		if restart == nil || restart() != nil {
			w.WriteHeader(500)
			return
		}
		observer = matrixHTTPPeer(t, "http://"+r.Host, credential)
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	mux.HandleFunc("/probe/recovery-arm", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Registration string }
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Registration == "" {
			w.WriteHeader(400)
			return
		}
		recoveryMu.Lock()
		recoveryTarget = in.Registration
		recoveryMu.Unlock()
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	mux.HandleFunc("/probe/recovery-state", func(w http.ResponseWriter, r *http.Request) {
		recoveryMu.Lock()
		defer recoveryMu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"waiting": recoveryWaiting, "disarmed": recoveryTarget == "", "holds": recoveryHolds, "admissions": admissions, "fresh": freshAdmissions, "registers": registerCalls})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	observer = matrixHTTPPeer(t, server.URL, credential)
	var registration broker.RegisterResult
	matrixRequire(t, observer.tool(t.Context(), "register_session", broker.RegisterRequest{Name: "peer", Harness: "codex", SessionID: matrixPeerSID, PID: os.Getpid(), CWD: data, ClientID: "steer-peer-device", ChannelID: "steer-peer-lease", RequestID: "steer-peer-request", Secret: clientstate.NewSecret()}, &registration) && registration.Code == "", "register peer")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var out broker.RegisterResult
				observer.tool(ctx, "renew_lease", broker.RenewRequest{RegistrationID: registration.Session.RegistrationID, ChannelID: "steer-peer-lease"}, &out)
			}
		}
	}()
	payload, _ := json.Marshal(map[string]string{"binary": binary, "token": token, "endpoint": server.URL + "/mcp?project=test", "origin": server.URL, "wrapped": os.Getenv("ASB_CODEX_STEER_WRAPPED_BINARY"), "cases": os.Getenv("ASB_CODEX_STEER_CASES"), "launcher": "../channel/testdata/codex-resume.sh", "diagnose": os.Getenv("ASB_CODEX_STEER_X1_DIAG")})
	driver := "testdata/codex_steer.py"
	if os.Getenv("ASB_CODEX_STEER_CROSS") == "1" {
		driver = "testdata/codex_cross.py"
	}
	command := exec.CommandContext(t.Context(), "python3", driver)
	command.Stdin = bytes.NewReader(payload)
	command.Stderr = io.Discard
	var out bytes.Buffer
	command.Stdout = &out
	err = command.Run()
	t.Log(out.String())
	if err != nil {
		t.Fatal("isolated steer E2E failed (sanitized stage above)")
	}
}

// A real released server supplies its own capability and schema negotiation.
func legacySteerHandler(t *testing.T, data, binary string) http.Handler {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	matrixRequire(t, err == nil, "reserve legacy loopback")
	address := listener.Addr().String()
	matrixRequire(t, listener.Close() == nil, "release legacy reservation")
	command := exec.Command(binary, "--listen", address, "--memory-dir", data)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	matrixRequire(t, command.Start() == nil, "legacy server start")
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = command.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	})
	endpoint := "http://" + address
	client := &http.Client{Timeout: time.Second}
	ready := false
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		response, err := client.Get(endpoint + "/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		select {
		case <-done:
			t.Fatal("legacy server exited during startup")
		case <-time.After(50 * time.Millisecond):
		}
	}
	matrixRequire(t, ready, "legacy server health")
	target, err := url.Parse(endpoint)
	matrixRequire(t, err == nil, "legacy loopback URL")
	return httputil.NewSingleHostReverseProxy(target)
}
