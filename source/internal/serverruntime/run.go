// Package serverruntime starts the HTTP relay without local harness execution.
package serverruntime

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"sync"
	"time"
)

// Run serves until cancellation. Help and version do not access disk or network.
func Run(ctx context.Context, args []string, version string, output io.Writer) (err error) {
	if output == nil {
		output = io.Discard
	}
	flags := flag.NewFlagSet("asb-server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", "127.0.0.1:8787", "HTTP listen address")
	memoryDir := flags.String("memory-dir", ".agent-memory", "project memory and server state directory")
	var trustedProxies []netip.Prefix
	flags.Func("trusted-proxy", "trusted TLS proxy CIDR (repeatable; default none); proxy must SET one X-Forwarded-Proto value", func(value string) error {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return errors.New("trusted proxy must be a CIDR")
		}
		trustedProxies = append(trustedProxies, prefix.Masked())
		return nil
	})
	showVersion := flags.Bool("version", false, "print version and exit")
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: asb-server [--listen address] [--memory-dir path] [--trusted-proxy CIDR]...")
		flags.SetOutput(output)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
		fmt.Fprintln(output, "AllowHTTP OFF behind any TLS proxy requires --trusted-proxy and a proxy that SETS X-Forwarded-Proto.")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &RunError{Reason: "flags_invalid", Flag: invalidFlag(args)}
	}
	if flags.NArg() != 0 {
		return &RunError{Reason: "flags_invalid"}
	}
	if *showVersion {
		_, err := fmt.Fprintf(output, "asb-server %s\n", version)
		if err != nil {
			return storageFailure(err)
		}
		return nil
	}
	directory, err := filepath.Abs(*memoryDir)
	if err != nil {
		return storageFailure(err)
	}
	serverdiag.Emit(serverdiag.Event{Type: "server-trusted-proxies", Count: uint64(len(trustedProxies))})
	handler, close, err := newManagedHandlerWithTrustedProxies(directory, nil, trustedProxies, version)
	if err != nil {
		return storageFailure(err)
	}
	defer func() {
		if closeErr := close(); closeErr != nil && err == nil {
			err = &RunError{Reason: "shutdown_failed"}
		}
	}()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return listenFailure(err)
	}
	defer listener.Close()
	serveCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &http.Server{Handler: protectRequests(handler), ErrorLog: serverdiag.ErrorLog(), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return serveCtx }}
	var connectionsMu sync.Mutex
	connections := make(map[net.Conn]http.ConnState)
	stopping := false
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		connectionsMu.Lock()
		defer connectionsMu.Unlock()
		switch state {
		case http.StateNew, http.StateActive:
			connections[conn] = state
			if stopping {
				if state == http.StateNew {
					_ = conn.Close()
				} else {
					_ = conn.SetReadDeadline(time.Now())
				}
			}
		default:
			delete(connections, conn)
		}
	}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	serverdiag.Emit(serverdiag.Event{Type: "server-listening", Bind: listener.Addr().(*net.TCPAddr).AddrPort().String()})
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return &RunError{Reason: "shutdown_failed"}
	case <-ctx.Done():
		cancel()
		// Cancellation does not unblock sockets reading headers or request bodies.
		connectionsMu.Lock()
		stopping = true
		for conn, state := range connections {
			if state == http.StateNew {
				_ = conn.Close()
			} else {
				_ = conn.SetReadDeadline(time.Now())
			}
		}
		connectionsMu.Unlock()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			_ = server.Close()
		}
		serveErr := <-result
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		if shutdownErr != nil || serveErr != nil {
			return &RunError{Reason: "shutdown_failed"}
		}
		return nil
	}
}
