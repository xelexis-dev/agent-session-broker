package serverruntime

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunShutdownWithUnusedConnection(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	dir := t.TempDir()
	var runErr error
	go func() {
		runErr = Run(ctx, []string{"--listen", address, "--memory-dir", dir}, "test", io.Discard)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(7 * time.Second):
			t.Error("runtime cleanup did not stop")
		}
	})
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	health := func() bool {
		res, err := client.Get("http://" + address + "/healthz")
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode == http.StatusOK
	}
	deadline := time.Now().Add(5 * time.Second)
	for !health() {
		if time.Now().After(deadline) {
			t.Fatal("runtime health unavailable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	unused, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unused.Close()
	// A subsequent fresh request passes the accept loop after the unused socket.
	if !health() {
		t.Fatal("runtime did not accept the following request")
	}
	cancel()
	select {
	case <-done:
		if runErr != nil {
			t.Fatalf("server shutdown with unused connection: %v", runErr)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("runtime did not stop")
	}
	_ = unused.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := unused.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("unused connection was not closed: %v", err)
	}
}
