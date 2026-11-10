package serverruntime

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestMisuseK3HostileConnectionsShutdown(t *testing.T) {
	for _, mode := range []string{"headerless", "tls-fragment", "slow-body"} {
		t.Run(mode, func(t *testing.T) {
			reserved, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := reserved.Addr().String()
			reserved.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, []string{"--listen", address, "--memory-dir", t.TempDir()}, "test", io.Discard)
			}()
			health := func() bool {
				c := http.Client{Timeout: 200 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
				r, e := c.Get("http://" + address + "/healthz")
				if e != nil {
					return false
				}
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
				return r.StatusCode == 200
			}
			until := time.Now().Add(5 * time.Second)
			for !health() {
				if time.Now().After(until) {
					t.Fatal("temporary runtime unavailable")
				}
				time.Sleep(10 * time.Millisecond)
			}
			var conns []net.Conn
			defer func() {
				for _, c := range conns {
					c.Close()
				}
			}()
			for i := 0; i < 24; i++ {
				c, e := net.DialTimeout("tcp", address, time.Second)
				if e != nil {
					t.Fatal(e)
				}
				conns = append(conns, c)
				switch mode {
				case "tls-fragment":
					_, e = c.Write([]byte{0x16, 0x03, 0x01, 0x01, 0x00, 0x01})
				case "slow-body":
					_, e = io.WriteString(c, "POST /setup HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 4096\r\n\r\npassword=")
				}
				if e != nil {
					t.Fatal(e)
				}
			}
			if !health() {
				t.Fatal("hostile connection accept barrier")
			}
			time.Sleep(30 * time.Millisecond)
			start := time.Now()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("runtime shutdown returned error: %v", err)
				}
				if time.Since(start) >= 5*time.Second {
					t.Fatal("shutdown exceeded5s")
				}
			case <-time.After(6 * time.Second):
				t.Fatal("shutdown stuck")
			}
		})
	}
}
