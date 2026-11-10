package serverruntime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

func TestRunPassesVersionToPanelSidebar(t *testing.T) {
	dir := t.TempDir()
	store, err := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	if err != nil {
		t.Fatal("store open")
	}
	const password = "synthetic-panel-version-password"
	if store.Setup(password) != nil || store.SaveProject("versions", "token", true, "") != nil || store.SetInstanceHTTPPolicy("admin", true) != nil {
		t.Fatal("fixture setup")
	}
	token, err := store.IssueToken("versions", "synthetic-name")
	if err != nil || store.Close() != nil {
		t.Fatal("token fixture failed")
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("port allocation")
	}
	address := reservation.Addr().String()
	reservation.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, []string{"--listen", address, "--memory-dir", dir}, "9.9.9-test", io.Discard) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("runtime shutdown failed")
			}
		case <-time.After(7 * time.Second):
			t.Error("runtime shutdown timed out")
		}
	})
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + address
	read := func(path string) (int, string) {
		res, e := client.Get(base + path)
		if e != nil {
			return 0, ""
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(raw)
	}
	deadline := time.Now().Add(5 * time.Second)
	var login string
	for time.Now().Before(deadline) {
		status, body := read("/login")
		if status == 200 {
			login = body
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(login)
	if len(csrf) != 2 {
		t.Fatal("login unavailable")
	}
	res, err := client.PostForm(base+"/login", url.Values{"username": {"admin"}, "password": {password}, "csrf": {csrf[1]}})
	if err != nil {
		t.Fatal("login request failed")
	}
	res.Body.Close()
	if res.StatusCode != 303 {
		t.Fatal("login rejected")
	}
	status, body := read("/")
	if status != 200 {
		t.Fatal("panel unavailable")
	}
	start := strings.Index(body, `<aside class="sidebar"`)
	end := strings.Index(body, `</aside>`)
	if start < 0 || end <= start || !strings.Contains(body[start:end], "9.9.9-test") {
		t.Fatal("server version absent from panel sidebar")
	}
	req, err := http.NewRequest("POST", base+"/mcp?project=versions", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	if err != nil {
		t.Fatal("initialize request failed")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	initialized, err := client.Do(req)
	if err != nil {
		t.Fatal("server initialize failed")
	}
	defer initialized.Body.Close()
	raw, _ := io.ReadAll(initialized.Body)
	var message struct {
		Result struct {
			ServerInfo struct {
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if initialized.StatusCode != 200 || json.Unmarshal(raw, &message) != nil || message.Result.ServerInfo.Version != "9.9.9-test" {
		t.Fatal("runtime version absent from MCP serverInfo")
	}

}
