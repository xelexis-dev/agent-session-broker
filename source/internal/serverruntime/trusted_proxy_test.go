package serverruntime

import (
	"bytes"
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustedProxyInvalidCIDRStopsBeforeStorage(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "bad", "10.0.0.0/99", ""} {
		t.Run(value, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "not-created")
			if err := Run(context.Background(), []string{"--memory-dir", dir, "--trusted-proxy", value}, "test", &bytes.Buffer{}); err == nil {
				t.Fatal("invalid proxy accepted")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("invalid proxy touched storage")
			}
		})
	}
}
func TestTrustedProxyHelpAndRepeatableSyntax(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"--help"}, "test", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "default none") || !strings.Contains(out.String(), "SETS X-Forwarded-Proto") {
		t.Fatal("proxy migration guidance missing")
	}
	out.Reset()
	if err := Run(context.Background(), []string{"--trusted-proxy", "127.0.0.1/32", "--trusted-proxy", "::1/128", "--version"}, "test", &out); err != nil {
		t.Fatal("repeatable proxy rejected")
	}
}

func TestTrustedProxyReachesManagedHandler(t *testing.T) {
	dir := t.TempDir()
	s, err := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Setup("synthetic-proxy-password") != nil || s.CreateProject("test") != nil {
		t.Fatal("seed")
	}
	raw, err := s.IssueToken("test", "proxy-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h, close, err := newManagedHandlerWithTrustedProxies(dir, nil, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, "0.17.3")
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"fixture","version":"0.17.3"}}}`
	r := httptest.NewRequest("POST", "/mcp?project=test", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer "+raw)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("X-Forwarded-Proto", "https")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	if out.Code != 200 {
		t.Fatalf("configured proxy did not reach managed panel: status=%d", out.Code)
	}
}
