package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Don-Works/brw/internal/testbed"
)

func main() {
	var config testbed.Config
	flag.StringVar(&config.Address, "http", "127.0.0.1:0", "Literal loopback IP and port for the page")
	flag.StringVar(&config.FrameAddress, "frame-http", "127.0.0.1:0", "Second loopback origin for cross-origin frames")
	flag.Int64Var(&config.Seed, "seed", 7, "Deterministic mutation seed")
	flag.IntVar(&config.Chaos, "chaos", 2, "Chaos level from 0 to 3")
	flag.IntVar(&config.MaxEvents, "max-events", 256, "Bounded events per run from 1 to 4096")
	flag.Parse()
	s, err := testbed.Start(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"url": s.URL(), "frame_url": s.FrameURL(), "seed": config.Seed, "max_events": config.MaxEvents}); err != nil {
		s.Close()
		os.Exit(1)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	if err = s.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
