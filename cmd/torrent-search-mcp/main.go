// Command torrent-search-mcp is the Go stdio MCP server for Torrent Search:
// a single static binary that drops in for `uvx torrent-search-mcp --mode stdio`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bonzofenix/torrent-search-mcp/internal/mcpserver"
)

// shutdownGrace bounds how long a signal-triggered shutdown waits for
// in-flight tool calls.
const shutdownGrace = 3 * time.Second

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// stdout carries JSON-RPC only; every log line goes to stderr.
	log.SetOutput(os.Stderr)
	log.SetPrefix("torrent-search-mcp: ")

	flags := flag.NewFlagSet("torrent-search-mcp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	mode := flags.String("mode", "stdio", "Mode to run in. Only stdio is supported by the Go binary.")
	showVersion := flags.Bool("version", false, "Print the version and exit.")
	// Accepted and ignored, so existing `torrent-search-mcp --mode stdio ...`
	// host configs written for the Python CLI start unchanged.
	flags.String("host", "0.0.0.0", "Ignored (Python CLI compatibility).")
	flags.Int("port", 8000, "Ignored (Python CLI compatibility).")
	flags.Bool("reload", false, "Ignored (Python CLI compatibility).")
	flags.Bool("dev", false, "Ignored (Python CLI compatibility).")
	flags.Int("workers", 1, "Ignored (Python CLI compatibility).")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *mode != "stdio" {
		log.Printf("mode %q is not supported by the Go binary; use the Python package (uvx torrent-search-mcp --mode %s)", *mode, *mode)
		os.Exit(2)
	}
	os.Exit(run())
}

func run() int {
	// A clean stop is only ever stdin closing or SIGINT/SIGTERM. A hangup
	// from a detached controlling terminal is not a reason to die, and a
	// closed stderr must surface as a write error rather than a SIGPIPE kill.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server := mcpserver.NewServer(mcpserver.NewTools(), version)
	log.Printf("Starting MCP server %s over stdio", version)
	stdout := &lockedWriter{w: os.Stdout}
	transport := &mcp.IOTransport{Reader: newFrameFilter(os.Stdin, stdout), Writer: stdout}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, transport) }()

	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		// Closing the session waits for in-flight tool calls, and a scrape
		// can run for tens of seconds; don't let that outlast the host's
		// SIGTERM grace period.
		select {
		case err = <-done:
		case <-time.After(shutdownGrace):
		}
	}
	switch {
	case err == nil, errors.Is(err, io.EOF), ctx.Err() != nil:
		log.Printf("MCP server stopped")
		return 0
	default:
		log.Printf("MCP server failed: %v", err)
		return 1
	}
}
