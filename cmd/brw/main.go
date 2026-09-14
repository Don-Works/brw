// Command brw is the per-action browser CLI: one verb, one request to a
// running brwd, one answer on stdout. It is deliberately separate from brwctl,
// which administers the machine (setup, doctor, packaging) rather than driving a
// page.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Don-Works/brw/internal/cli"
)

func main() {
	// Ctrl-C during a long wait cancels the in-flight request instead of
	// leaving the daemon to finish an action nobody is waiting for.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
