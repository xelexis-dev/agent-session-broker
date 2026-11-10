package main

import (
	"context"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverruntime"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := serverruntime.Run(ctx, os.Args[1:], version, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
