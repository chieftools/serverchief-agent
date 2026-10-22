package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	// Embedded Mozilla roots for hosts without a usable CA bundle. The agent's
	// own HTTP client also merges them into the system pool (agent.RootCAs).
	_ "golang.org/x/crypto/x509roots/fallback"

	"go.chief.tools/serverchief-agent/cmd"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.NewRootCommand(version).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "serverchief:", err)
		os.Exit(1)
	}
}
