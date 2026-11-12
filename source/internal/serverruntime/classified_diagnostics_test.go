package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunFailuresHaveSafeClassifications(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listener fixture failed")
	}
	defer occupied.Close()
	for _, tc := range []struct {
		args                 []string
		reason, detail, flag string
	}{
		{[]string{"--version=fixture-secret"}, "flags_invalid", "", "version"},
		{[]string{"--fixture-secret"}, "flags_invalid", "", ""},
		{[]string{"--listen", occupied.Addr().String(), "--memory-dir", t.TempDir()}, "listen_failed", "addr_in_use", ""},
		{[]string{"--listen", "fixture-secret", "--memory-dir", t.TempDir()}, "listen_failed", "invalid_address", ""},
	} {
		err := Run(context.Background(), tc.args, "test", io.Discard)
		var classified interface {
			Classification() (string, string, string)
		}
		if !errors.As(err, &classified) {
			t.Error("Run did not return a typed classified error")
			continue
		}
		reason, detail, flag := classified.Classification()
		if reason != tc.reason || detail != tc.detail || flag != tc.flag || strings.Contains(err.Error(), "fixture-secret") {
			t.Error("unsafe or incorrect failure classification")
		}
	}
}
func TestRunListenEventProvidesCanonicalBoundPort(t *testing.T) {
	output := captureDiagnostics(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"--listen", "127.0.0.1:0", "--memory-dir", t.TempDir()}, "test", io.Discard)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("Run did not stop")
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(output.String(), "\n") {
			var e struct{ Type, Bind string }
			if json.Unmarshal([]byte(line), &e) == nil && e.Type == "server-listening" && e.Bind != "" {
				addr, err := net.ResolveTCPAddr("tcp", e.Bind)
				if err != nil || !addr.IP.IsLoopback() || addr.Port == 0 {
					t.Fatal("unsafe bound address")
				}
				response, err := http.Get("http://" + e.Bind + "/healthz")
				if err != nil {
					t.Fatal("event port unreachable")
				}
				response.Body.Close()
				if response.StatusCode != 200 {
					t.Fatal("event port health failed")
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listening event did not expose bound port")
}
func TestReviewSinkReferenceCanaries(t *testing.T) {
	for _, src := range []string{
		`package p;import "net/http";func f(){_ = new(http.Server)}`,
		`package p;import "net/http";var s http.Server`,
		`package p;import "net/http";type s = http.Server`,
		`package p;import "net/http";func f(){http.ListenAndServe("",nil)}`,
		`package p;import "net/http";func f(){http.Serve(nil,nil)}`,
		`package p;import "log";var logf=log.Printf`,
		`package p;import "fmt";func f(){p:=fmt.Println;_ = p}`,
		`package p;import "syscall";var stderr=syscall.Stderr`,
		`package p;import "os";func f(){_ = os.NewFile(2,"stderr")}`,
		`package p;import "runtime/debug";func f(){debug.PrintStack()}`,
	} {
		sinks, err := sinkCalls("internal/fixture_windows.go", []byte(src))
		if err != nil || len(sinks) == 0 {
			t.Error("review output sink bypass not rejected")
		}
	}
}

func TestClassifiedStorageAndShutdownErrorsNeverFormatRawError(t *testing.T) {
	raw := &os.PathError{Op: "open", Path: "fixture-private-path", Err: os.ErrPermission}
	if e := storageFailure(raw); e.Reason != "data_dir_permission" {
		t.Fatal("permission classification lost")
	}
	if e := listenFailure(raw); e.Detail != "permission" {
		t.Fatal("listen permission classification lost")
	}
	if e := ErrorEvent(&RunError{Reason: "shutdown_failed"}); e.Reason != "shutdown_failed" {
		t.Fatal("shutdown labelled as startup")
	}
	dir := t.TempDir()
	s, err := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	if err != nil {
		t.Fatal("lock fixture failed")
	}
	defer s.Close()
	var e *RunError
	err = Run(context.Background(), []string{"--memory-dir", dir, "--listen", "127.0.0.1:0"}, "test", io.Discard)
	if !errors.As(err, &e) || e.Reason != "data_dir_locked" {
		t.Fatal("data directory lock not classified")
	}
}
