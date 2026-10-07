// Command brw is the per-action browser CLI: one verb, one request to a running brwd, one answer on stdout.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Don-Works/brw/internal/cli"
)

func main() {

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
