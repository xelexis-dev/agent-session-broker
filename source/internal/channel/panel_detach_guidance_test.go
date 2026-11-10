package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestPanelDetachLiveAutoSessionStopsWithNotFoundGuidance(t *testing.T) {
	testPanelDetachOrder(t, false)
}

func TestPanelDetachAfterObservedDetachKeepsDetachedGuidance(t *testing.T) {
	testPanelDetachOrder(t, true)
}

func testPanelDetachOrder(t *testing.T, observed bool) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	id := broker.AttachRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-000000003221", PID: 3221, CWD: "/synthetic/detached"}
	target := r.identityHarness(t, dir, id)
	target.attach(t)
	original := target.session.RegistrationID
	endpoint, _ := url.Parse(r.endpoint)
	base := endpoint.Scheme + "://" + endpoint.Host
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	csrfRE := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	csrf := func(path string) string {
		t.Helper()
		started := time.Now()
		res, err := browser.Get(base + path)
		if err != nil {
			t.Fatalf("panel page unavailable: %s", panelHTTPError(err, time.Since(started), 0))
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		match := csrfRE.FindSubmatch(data)
		if len(match) != 2 {
			t.Fatalf("panel csrf missing for %s (HTTP %d)", path, res.StatusCode)
		}
		return string(match[1])
	}
	post := func(path string, values url.Values) {
		t.Helper()
		started := time.Now()
		res, err := browser.PostForm(base+path, values)
		if err != nil {
			t.Fatalf("panel post unavailable: %s", panelHTTPError(err, time.Since(started), 0))
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 303 {
			t.Fatalf("panel mutation status %d", res.StatusCode)
		}
	}
	post("/login", url.Values{"username": {"admin"}, "password": {"synthetic-detached-password"}, "csrf": {csrf("/login")}})
	var saved clientstate.SessionState
	readSaved := func() bool {
		paths, _ := filepath.Glob(filepath.Join(dir, "sessions", "*", "conn.json"))
		if len(paths) != 1 {
			return false
		}
		data, err := os.ReadFile(paths[0])
		return err == nil && json.Unmarshal(data, &saved) == nil
	}
	if !readSaved() {
		t.Fatal("initial saved proof unavailable")
	}
	originalSecret := saved.Registration.Secret
	var manager *detachedHarness
	if observed {
		manager = r.harness(t, t.TempDir(), 3222)
		manager.attach(t)
	}
	// Force a renew to have selected the previous gate before its replacement.
	earlierHeld, earlierRelease := make(chan struct{}), make(chan struct{})
	var earlierOnce, earlierReleaseOnce sync.Once
	var earlierRequest atomic.Pointer[http.Request]
	var earlierReturned atomic.Bool
	var earlierStatus atomic.Int32
	var earlierOutcome atomic.Int32 // 0 pending, 1 forwarded, 2 abortedCtx
	started := time.Now()
	var earlierHeldMs, earlierReleasedMs atomic.Int64
	completed := func(req *http.Request, status int) {
		if req == earlierRequest.Load() {
			earlierStatus.Store(int32(status))
			earlierReturned.Store(true)
		}
	}
	r.requestCompleted.Store(&completed)
	defer r.requestCompleted.Store(nil)
	releaseEarlier := func() {
		earlierReleaseOnce.Do(func() { earlierReleasedMs.Store(time.Since(started).Milliseconds()); close(earlierRelease) })
	}
	defer releaseEarlier()
	priorGate := func(w http.ResponseWriter, req *http.Request) bool {
		if req.URL.Path != "/mcp" || req.Method != "POST" {
			return false
		}
		data, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(strings.NewReader(string(data)))
		var rpc struct {
			Params struct {
				Name      string
				Arguments struct {
					RegistrationID string `json:"registrationId"`
				}
			}
		}
		_ = json.Unmarshal(data, &rpc)
		if rpc.Params.Name == "renew_lease" && rpc.Params.Arguments.RegistrationID == original {
			first := false
			earlierOnce.Do(func() {
				first = true
				earlierRequest.Store(req)
				earlierHeldMs.Store(time.Since(started).Milliseconds())
				close(earlierHeld)
			})
			if first {
				select {
				case <-earlierRelease:
				case <-req.Context().Done():
					earlierOutcome.Store(2)
					return true
				}
				if req.Context().Err() != nil {
					earlierOutcome.Store(2)
					return true
				}
				earlierOutcome.Store(1)
			}
		}
		return false
	}
	r.installGate(&priorGate)
	defer r.attachmentGate.Store(nil)
	waitFor(t, "earlier target renew selected prior gate", func() bool {
		select {
		case <-earlierHeld:
			return true
		default:
			return false
		}
	})
	before := r.direct.Load()
	if observed {
		held, release := make(chan struct{}), make(chan struct{})
		var first, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		gate := func(w http.ResponseWriter, req *http.Request) bool {
			if req.URL.Path != "/mcp" || req.Method != "POST" {
				return false
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return true
			}
			req.Body = io.NopCloser(strings.NewReader(string(body)))
			var rpc struct{ Params struct{ Name string } }
			if json.Unmarshal(body, &rpc) != nil || rpc.Params.Name != "report_ingress_failure" {
				return false
			}
			capture := false
			first.Do(func() { capture = true; close(held) })
			if capture {
				select {
				case <-release:
				case <-req.Context().Done():
					return true
				}
			}
			return false
		}
		// Deliberately omit draining here: the earlier renew can observe detached after gate replacement.
		r.installGate(&gate)
		defer r.attachmentGate.Store(nil)
		if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": original}, nil); err != nil {
			t.Fatal("manager detach failed")
		}
		releaseEarlier()
		waitFor(t, "held final report after detached observation", func() bool {
			select {
			case <-held:
				return readSaved() && saved.Registration.Detached
			default:
				return false
			}
		})
		// Existing detached-row cleanup removes this row on list GET; a later panel POST is refused (403).
		response, err := browser.Get(base + "/projects/sessions?project=detach")
		if err != nil {
			t.Fatal("cleanup page unavailable")
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("cleanup page status %d", response.StatusCode)
		}
		unblock()
	} else {
		// Only calls that can save Detached are held; reconnect and receive traffic must pass.
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		parked := make(chan struct{})
		var parkedOnce sync.Once
		installed := time.Now()
		var parkedMs atomic.Int64
		keys := []string{"rpc.initialize", "rpc.tools_list", "call.renew_lease", "call.resume_session", "call.register_session", "call.wait_for_message", "call.session_token_info", "call.report_ingress_failure", "call.probe_response", "call.other", "oauth.session_registration", "oauth.other", "get"}
		counts := make(map[string]*detachGateCounts, len(keys))
		for _, key := range keys {
			counts[key] = &detachGateCounts{}
		}
		renews, resumes, direct := r.renews.Load(), r.resumes.Load(), r.direct.Load()
		earlierDone := func() bool {
			req := earlierRequest.Load()
			return earlierReturned.Load() || (req != nil && req.Context().Err() != nil) || earlierOutcome.Load() == 2
		}
		t.Cleanup(func() {
			if !t.Failed() {
				return
			}
			state := map[string]any{"gateInstalledMs": installed.Sub(started).Milliseconds(), "parkedMs": parkedMs.Load(), "parked": false}
			select {
			case <-parked:
				state["parked"] = true
			default:
			}
			for _, key := range keys {
				c := counts[key]
				state[key] = map[string]int32{"arrived": c.arrived.Load(), "held": c.held.Load(), "forwarded": c.forwarded.Load(), "abortedCtx": c.aborted.Load()}
			}
			outcome := "pending"
			switch earlierOutcome.Load() {
			case 1:
				outcome = "forwarded"
			case 2:
				outcome = "abortedCtx"
			}
			if req := earlierRequest.Load(); req != nil && req.Context().Err() != nil {
				outcome = "abortedCtx"
			}
			backendStatus := earlierStatus.Load()
			if earlierOutcome.Load() != 1 {
				backendStatus = 0
			}
			state["E"] = map[string]any{"heldMs": earlierHeldMs.Load(), "releasedMs": earlierReleasedMs.Load(), "outcome": outcome, "backendStatus": backendStatus, "handlerReturned": earlierReturned.Load()}
			var status authStatus
			diagnosticCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = call(diagnosticCtx, target.client, "auth_status", map[string]any{}, &status)
			state["client"] = map[string]any{"status": detachStatusCode(status.Status), "registered": status.Registered,
				"registration-detached": strings.Contains(status.Message, "registration-detached"), "registration-not-found": strings.Contains(status.Message, "registration-not-found"), "credential-rejected": strings.Contains(status.Message, "credential-rejected"), "automatic-reauthorization-stopped": strings.Contains(status.Message, "automatic-reauthorization-stopped")}
			if readSaved() {
				reason := "none"
				if saved.AutoRecovery != nil && saved.AutoRecovery.StopReason != "" {
					reason = "other"
					switch saved.AutoRecovery.StopReason {
					case "registration-detached", "registration-not-found", "credential-rejected", "automatic-reauthorization-stopped":
						reason = saved.AutoRecovery.StopReason
					}
				}
				state["conn"] = map[string]any{"registrationDetached": saved.Registration.Detached, "stopReason": reason, "credentialPresent": saved.Registration.Secret != ""}
			}
			state["runtime"] = map[string]int32{"renews": r.renews.Load() - renews, "resumes": r.resumes.Load() - resumes, "direct": r.direct.Load() - direct}
			data, _ := json.Marshal(state)
			t.Logf("detach-barrier-state %s", data)
		})
		gate := func(w http.ResponseWriter, req *http.Request) bool {
			key, held := detachGateRequest(req, original)
			c := counts[key]
			c.arrived.Add(1)
			if held {
				c.held.Add(1)
				parkedOnce.Do(func() { parkedMs.Store(time.Since(installed).Milliseconds()); close(parked) })
				select {
				case <-release:
				case <-req.Context().Done():
					c.aborted.Add(1)
					return true
				}
				if req.Context().Err() != nil {
					c.aborted.Add(1)
					return true
				}
			}
			c.forwarded.Add(1)
			return false
		}
		pending := r.installGate(&gate)
		// The forced earlier request must exist in the old gate's installation snapshot.
		done, exists := pending[earlierRequest.Load()]
		if !exists {
			t.Fatal("forced earlier request missing from installation snapshot")
		}
		select {
		case <-done:
			t.Fatal("forced earlier request completed before gate installation")
		default:
		}
		releaseEarlier()
		waitFor(t, "next target renew parked", func() bool {
			select {
			case <-parked:
				return true
			default:
				return false
			}
		})
		// One parked sequential renew proves its predecessor completed or was abandoned.
		assertBarrier := func() {
			renew, resume := counts["call.renew_lease"], counts["call.resume_session"]
			if renew.held.Load()+resume.held.Load() < 1 || renew.forwarded.Load()+resume.forwarded.Load() != 0 || !earlierDone() {
				t.Fatal("detach barrier invariant failed")
			}
		}
		csrfValue := csrf("/projects/sessions?project=detach")
		assertBarrier()
		post("/sessions/detach", url.Values{"project": {"detach"}, "id": {original}, "csrf": {csrfValue}})
		unblock()
	}
	waitFor(t, "automatic recovery stopped after panel detach", func() bool { return readSaved() && saved.AutoRecovery != nil && saved.AutoRecovery.StopReason != "" })
	reason := "registration-not-found"
	expectedExchanges := int32(1)
	if observed {
		reason = "registration-detached"
		expectedExchanges = 0
	}
	if saved.AutoRecovery.StopReason != reason {
		t.Fatalf("wrong stop reason after panel detach: %s", saved.AutoRecovery.StopReason)
	}
	// Persisting StopReason precedes the recovery worker releasing its slot.
	waitFor(t, "terminal auth status after panel detach", func() bool {
		status := target.status(t)
		if observed {
			return !status.Registered && strings.Contains(status.Message, reason) && strings.Contains(status.Message, "attach_session")
		}
		return strings.Contains(status.Message, "automatic-reauthorization-stopped: "+reason)
	})
	for i := 0; i < 3; i++ {
		status := target.status(t)
		if status.Registered || !strings.Contains(status.Message, reason) || !strings.Contains(status.Message, "attach_session") || strings.Contains(status.Message, "identity-conflict") {
			t.Fatal("panel detach auth_status lacks concrete detached guidance")
		}
		if !observed && (!strings.Contains(status.Message, "may have detached") || !strings.Contains(status.Message, "another session may have retired it")) {
			t.Fatal("not-found guidance lost")
		}
		if observed && !strings.Contains(status.Message, "administrator detached") {
			t.Fatal("detached guidance lost")
		}
	}
	if saved.Registration.RegistrationID != original || saved.Registration.Secret != originalSecret {
		t.Fatal("panel detach replaced/lost saved proof")
	}
	count := r.direct.Load()
	if count != before+expectedExchanges {
		t.Fatalf("unexpected terminal direct exchange count, got %d", count-before)
	}
	time.Sleep(150 * time.Millisecond)
	_ = target.status(t)
	if r.direct.Load() != count {
		t.Fatal("terminal detach retried direct admission")
	}
}

// Preserve streaming while observing only the numeric response status.
type detachResponseStatus struct {
	http.ResponseWriter
	status int
}

func (w *detachResponseStatus) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *detachResponseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *detachResponseStatus) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }

type detachGateCounts struct{ arrived, held, forwarded, aborted atomic.Int32 }

func detachGateRequest(req *http.Request, target string) (string, bool) {
	if req.Method == http.MethodGet {
		return "get", false
	}
	if req.URL.Path == "/oauth/session_registration" {
		return "oauth.session_registration", false
	}
	if strings.HasPrefix(req.URL.Path, "/oauth/") {
		return "oauth.other", false
	}
	if req.URL.Path != "/mcp" || req.Method != http.MethodPost {
		return "call.other", false
	}
	data, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(strings.NewReader(string(data)))
	var rpc struct {
		Method string
		Params struct {
			Name      string
			Arguments struct {
				RegistrationID string `json:"registrationId"`
				SessionID      string `json:"sessionId"`
			}
		}
	}
	_ = json.Unmarshal(data, &rpc)
	switch rpc.Method {
	case "initialize":
		return "rpc.initialize", false
	case "tools/list":
		return "rpc.tools_list", false
	case "tools/call":
		switch rpc.Params.Name {
		case "renew_lease", "resume_session":
			return "call." + rpc.Params.Name, rpc.Params.Arguments.RegistrationID == target || (rpc.Params.Name == "resume_session" && rpc.Params.Arguments.SessionID == target)
		case "register_session", "wait_for_message", "session_token_info", "report_ingress_failure", "probe_response":
			return "call." + rpc.Params.Name, false
		}
	}
	return "call.other", false
}

func detachStatusCode(status string) string {
	switch status {
	case "authorized", "ready", "pending", "unauthorized", "error", "unsupported-harness", "expired", "stopped":
		return status
	case "":
		return "none"
	default:
		return "other"
	}
}

func TestDetachGateOnlyHoldsTargetRenewAndResume(t *testing.T) {
	for _, tc := range []struct {
		method, path, body, key string
		hold                    bool
	}{
		{"POST", "/mcp", `{"method":"tools/call","params":{"name":"renew_lease","arguments":{"registrationId":"target"}}}`, "call.renew_lease", true},
		{"POST", "/mcp", `{"method":"tools/call","params":{"name":"resume_session","arguments":{"sessionId":"target"}}}`, "call.resume_session", true},
		{"POST", "/mcp", `{"method":"tools/call","params":{"name":"renew_lease","arguments":{"registrationId":"other"}}}`, "call.renew_lease", false},
		{"POST", "/mcp", `{"method":"initialize"}`, "rpc.initialize", false},
		{"POST", "/mcp", `{"method":"tools/list"}`, "rpc.tools_list", false},
		{"POST", "/mcp", `{"method":"tools/call","params":{"name":"wait_for_message"}}`, "call.wait_for_message", false},
		{"POST", "/mcp", `{"method":"tools/call","params":{"name":"session_token_info"}}`, "call.session_token_info", false},
		{"POST", "/mcp", `{"method":"tools/call","params":{"name":"report_ingress_failure"}}`, "call.report_ingress_failure", false},
		{"POST", "/mcp", `{"method":"tools/call","params":{"name":"probe_response"}}`, "call.probe_response", false},
		{"POST", "/oauth/session_registration", "", "oauth.session_registration", false},
		{"POST", "/oauth/token", "", "oauth.other", false},
		{"GET", "/mcp", "", "get", false},
	} {
		req, _ := http.NewRequest(tc.method, "http://synthetic"+tc.path, strings.NewReader(tc.body))
		key, hold := detachGateRequest(req, "target")
		if key != tc.key || hold != tc.hold {
			t.Fatal("gate request classification mismatch")
		}
		body, _ := io.ReadAll(req.Body)
		if string(body) != tc.body {
			t.Fatal("gate consumed request body")
		}
	}
}
