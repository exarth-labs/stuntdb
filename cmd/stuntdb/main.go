// Command stuntdb makes small, safe, realistic copies of production databases.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/exarth-labs/stuntdb/internal/cli"
	"github.com/exarth-labs/stuntdb/internal/errs"

	// Database adapters register themselves when imported.
	_ "github.com/exarth-labs/stuntdb/dialects/sqlite"
)

// Exit codes are part of stuntdb's public contract: never renumber an existing code.
const (
	exitOK           = 0
	exitError        = 1
	exitUsage        = 2
	exitCheckFailed  = 3
	exitConnect      = 4
	exitIntegrity    = 5
	exitLeak         = 6
	exitOutput       = 7
	exitIncompatible = 8
	exitInterrupted  = 130
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return exitCode(ctx, cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// exitCode is the only place where errors become exit codes.
func exitCode(ctx context.Context, err error) int {
	if err == nil {
		return exitOK
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return exitInterrupted
	}
	switch errs.KindOf(err) {
	case errs.Usage:
		return exitUsage
	case errs.CheckFailed:
		return exitCheckFailed
	case errs.Connect:
		return exitConnect
	case errs.Integrity:
		return exitIntegrity
	case errs.Leak:
		return exitLeak
	case errs.Output:
		return exitOutput
	case errs.Incompatible:
		return exitIncompatible
	default:
		return exitError
	}
}
