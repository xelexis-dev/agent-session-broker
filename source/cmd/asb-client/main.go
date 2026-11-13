package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/channel"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "codex-resume-helper" {
		if err := channel.RunCodexResumeHelper(ctx, args[1:]); err != nil {
			if !errors.Is(err, channel.ErrCodexResumeIncomplete) {
				fmt.Fprintln(os.Stderr, err)
			}
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && args[0] == "kimi-hook" {
		if err := channel.RunKimiHook(ctx, args[1:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	if len(args) == 1 {
		switch args[0] {
		case "--version", "-version":
			fmt.Printf("asb-client %s\n", version)
			return
		case "--help", "-help", "-h":
			fmt.Println("Usage: asb-client --broker-url URL (--token value | --token-file path) [--auth-dir path] [--nickname name] [--codex-steer on|off] [--tool-profile full|worker]\nConnect local Claude, Codex or Kimi and automatically register the verified current conversation.\n  --token value     token: an asb_p_ project token (HTTPS broker only; direct registration when advertised and permitted, otherwise manual approval) or a session token\n  --token-file path   absolute path of a private regular file holding the token (POSIX: owner-only mode)\n  Without the flags: AGENT_SESSION_BROKER_URL, AGENT_SESSION_BROKER_TOKEN, then AGENT_SESSION_BROKER_TOKEN_FILE; the client clears them at start.\n  --auth-dir path   local directory for session credentials and registration state (alias: --state-dir)\n  --nickname name   requested direct-registration nickname; server may return a fallback\n  --codex-steer on|off   daemon Codex steering (default: on)\n  --tool-profile full|worker   tool list profile (default: full; authorization unchanged)\n  --version   print version and exit\n  --help      print help and exit")
			return
		}
	}
	// The client parser owns available options; reject positional arguments here.
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tool-profile", "-tool-profile", "--codex-steer", "-codex-steer", "--nickname", "-nickname", "--broker-url", "-broker-url", "--token", "-token", "--token-file", "-token-file", "--state-dir", "-state-dir", "--auth-dir", "-auth-dir":
			i++
			continue
		}
		if len(args[i]) == 0 || args[i][0] != '-' {
			fmt.Fprintln(os.Stderr, "unexpected positional argument")
			os.Exit(2)
		}
	}
	if err := channel.RunClient(ctx, args, version); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
