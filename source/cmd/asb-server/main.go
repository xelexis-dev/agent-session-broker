package main

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverruntime"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := serverruntime.Run(ctx, os.Args[1:], version, os.Stdout); err != nil {
		serverdiag.Emit(serverruntime.ErrorEvent(err))
		drain, stop := context.WithTimeout(context.Background(), 2*time.Second)
		serverdiag.Shutdown(drain)
		stop()
		os.Exit(1)
	}
	drain, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	serverdiag.Shutdown(drain)
}
