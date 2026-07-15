// Command bitch is the Telegram↔Claude-Code bridge described in DESIGN.md.
//
// Two subcommands:
//
//	bitch serve   run the bridge (default): Telegram poll loop + agent turns.
//	bitch hook    PreToolUse permission hook; invoked by Claude Code, it calls
//	              back into the running bridge to apply the tier policy (§4.3).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/intob/bitch/internal/bridge"
	"github.com/intob/bitch/internal/config"
	"github.com/intob/bitch/internal/store"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "hook":
		os.Exit(runHook())
	case "serve":
		runServe()
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, "usage: bitch [serve|hook]")
	default:
		// No subcommand — default to serve (systemd invokes `bitch` bare).
		runServe()
	}
}

func runServe() {
	cfg, err := config.Load()
	if err != nil {
		fatalf("config: %v", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		fatalf("store: %v", err)
	}
	defer st.Close()

	exe, err := os.Executable()
	if err != nil {
		fatalf("locate executable: %v", err)
	}
	exe, _ = filepath.Abs(exe)

	br, err := bridge.New(cfg, st, exe)
	if err != nil {
		fatalf("bridge: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := br.Run(ctx); err != nil && err != context.Canceled {
		fatalf("run: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bitch: "+format+"\n", args...)
	os.Exit(1)
}
