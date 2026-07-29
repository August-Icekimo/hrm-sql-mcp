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
	fmt.Fprintf(os.Stderr, "hrm-sql-mcp: profile=%s targets=%v\n  audit=%s\n  approvals=%s\n  snapshots=%s\n",
		svc.Policy().Profile, svc.Aliases(), svc.AuditPath(),
		svc.Approvals().Dir(), svc.Snapshots().Dir())

	// The client closes stdin to shut us down, which Run already handles.
	// Catching signals as well means a terminal Ctrl-C during manual testing
	// gets the same clean path, including closing the audit log.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Write tools are added here rather than inside New, so that the set of
	// tools that can change the database is visible at a call site instead of
	// buried in a registration list.
	srv := mcpserver.New(svc)
	mcpserver.AddWriteTools(srv, svc)
	return mcpserver.Run(ctx, srv)
}
