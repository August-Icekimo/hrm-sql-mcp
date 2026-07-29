package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/codex-k8s/hrm-sql-mcp/internal/mcpserver"
	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	// stdout is the MCP transport, so every human-readable byte goes to
	// stderr. A stray Println here would corrupt the JSON-RPC stream, and the
	// client would report a protocol error with no hint of the real cause.
	fmt.Fprintf(os.Stderr, "hrm-sql-mcp: profile=%s targets=%v audit=%s\n",
		svc.Policy().Profile, svc.Aliases(), svc.AuditPath())

	// The client closes stdin to shut us down, which Run already handles.
	// Catching signals as well means a terminal Ctrl-C during manual testing
	// gets the same clean path, including closing the audit log.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return mcpserver.Run(ctx, mcpserver.New(svc))
}
