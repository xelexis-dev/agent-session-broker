package serverruntime

import (
	"bytes"
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerFlagErrorsDoNotExposeInput(t *testing.T) {
	secret := "fixture-secret-request-value"
	for _, args := range [][]string{{"--" + secret}, {"--trusted-proxy", secret}, {"--version=" + secret}, {secret}, {"--listen", secret, "--memory-dir", t.TempDir()}} {
		var out bytes.Buffer
		err := Run(context.Background(), args, "test", &out)
		if err == nil {
			t.Fatal("expected rejected configuration")
		}
		if strings.Contains(out.String(), secret) || strings.Contains(err.Error(), secret) {
			t.Error("configuration diagnostic disclosed input")
		}
	}
}

func TestServerRequestPanicAndErrorLogWholeOutput(t *testing.T) {
	output := captureDiagnostics(t)
	secret := "fixture-request-secret"
	h := protectRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(secret) }))
	server := httptest.NewUnstartedServer(h)
	server.Config.ErrorLog = serverdiag.ErrorLog()
	server.Start()
	defer server.Close()
	request, _ := http.NewRequest("POST", server.URL+"/"+secret+"?q="+secret, strings.NewReader(secret))
	request.Host = secret
	for _, header := range []string{"Authorization", "Cookie", "User-Agent", "X-Forwarded-For", "Forwarded", "X-Forwarded-Proto", "X-Extra", "Content-Type", "Accept", "Accept-Encoding", "X-ASB-Client-Transport", "X-XBroker-Attachment-Name", "Mcp-Session-Id", "Mcp-Protocol-Version", "Origin", "Referer"} {
		request.Header.Set(header, secret)
	}
	response, _ := server.Client().Do(request)
	if response != nil {
		response.Body.Close()
	}
	server.Config.ErrorLog.Print(secret)
	if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), "server_request_panic") {
		t.Fatal("whole-output panic protection failed")
	}
	// ErrAbortHandler does not produce a second panic diagnostic.
	before := strings.Count(output.String(), "server_request_panic")
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Fatal("abort marker not preserved")
			}
		}()
		protectRequests(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	if strings.Count(output.String(), "server_request_panic") != before {
		t.Fatal("abort reported as panic")
	}
}

// A separate process captures the real stdout/stderr and default HTTP logger.
func TestServerWholeProcessOutputCanary(t *testing.T) {
	if os.Getenv("ASB_OUTPUT_CANARY_PROCESS") == "1" {
		runOutputCanaryChild(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestServerWholeProcessOutputCanary$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ASB_OUTPUT_CANARY_PROCESS=1")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal("whole-output child failed")
	}
	if bytes.Contains(raw, []byte("fixture-request-secret")) || bytes.Contains(raw, []byte("asb_s_")) {
		t.Fatal("request secret reached stdout/stderr/ErrorLog")
	}
	if !bytes.Contains(raw, []byte("http-transport-denied")) || !bytes.Contains(raw, []byte("server_request_panic")) {
		t.Fatal("whole-output canary did not exercise diagnostic sinks")
	}
}
func runOutputCanaryChild(t *testing.T) {
	secret := "asb_s_" + strings.Repeat("fixture-request-secret", 3)
	directory, err := os.MkdirTemp("", secret)
	if err != nil {
		t.Fatal("temporary directory unavailable")
	}
	defer os.RemoveAll(directory)
	seed, err := controlpanel.OpenSQLite(filepath.Join(directory, "control"))
	if err != nil {
		t.Fatal("seed init failed")
	}
	if seed.Setup("synthetic-output-canary-password") != nil || seed.CreateProject("fixture-request-secret") != nil || seed.SetInstanceHTTPPolicy("admin", false) != nil {
		t.Fatal("seed setup failed")
	}
	if seed.Close() != nil {
		t.Fatal("seed close failed")
	}
	h, closeHandler, err := newManagedHandler(directory, nil)
	if err != nil {
		t.Fatal("handler init failed")
	}
	defer closeHandler()
	server := httptest.NewUnstartedServer(protectRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic(secret)
		}
		h.ServeHTTP(w, r)
	})))
	server.Config.ErrorLog = serverdiag.ErrorLog()
	server.Start()
	defer server.Close()
	for _, path := range []string{"/mcp", "/attachments/" + secret, "/assets/" + secret, "/api/v1/projects/" + secret, "/" + secret, "/panic"} {
		request, _ := http.NewRequest("POST", server.URL+path+"?project=fixture-request-secret&q="+secret, strings.NewReader(secret))
		request.Host = secret
		for _, header := range []string{"Authorization", "Cookie", "User-Agent", "X-Forwarded-For", "Forwarded", "X-Forwarded-Proto", "X-Extra", "Content-Type", "Accept", "Accept-Encoding", "X-ASB-Client-Transport", "X-XBroker-Attachment-Name", "Mcp-Session-Id", "Mcp-Protocol-Version", "Origin", "Referer"} {
			request.Header.Set(header, secret)
		}
		response, _ := server.Client().Do(request)
		if path != "/panic" && (response == nil || response.StatusCode != http.StatusForbidden) {
			t.Fatal("malformed Origin canary was not rejected")
		}
		if response != nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}

	// Keep malformed-Origin cases above; these independently prove transport reachability.
	for _, origin := range []string{"", "http://" + secret} {
		request, err := http.NewRequest("POST", server.URL+"/attachments/"+secret+"?project=fixture-request-secret&q="+secret, strings.NewReader(secret))
		if err != nil {
			t.Fatal("transport canary request invalid")
		}
		request.Host = secret
		for _, header := range []string{"Authorization", "Cookie", "User-Agent", "X-Forwarded-For", "Forwarded", "X-Forwarded-Proto", "X-Extra", "Content-Type", "Accept", "Accept-Encoding", "X-ASB-Client-Transport", "X-XBroker-Attachment-Name", "Mcp-Session-Id", "Mcp-Protocol-Version", "Referer"} {
			request.Header.Set(header, secret)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal("transport canary request failed")
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusForbidden || !bytes.Contains(body, []byte("http_not_allowed")) {
			t.Fatal("canary did not reach denyTransport")
		}
	}
	for _, args := range [][]string{{"--" + secret}, {"--trusted-proxy", secret}, {"--version=" + secret}, {"--memory-dir", filepath.Join(directory, "missing", secret), "--listen", secret}} {
		if err := Run(context.Background(), args, "test", os.Stdout); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	server.Config.ErrorLog.Print(secret)
	drain, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	serverdiag.Shutdown(drain)
}
