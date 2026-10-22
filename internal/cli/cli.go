// Package cli implements stuntdb's command-line interface. It parses flags,
// calls the application layer and renders results; it holds no business logic.
//
// Results go to stdout and messages go to stderr, so that stdout stays clean
// for scripts and pipes. Every error is redacted before it is printed or
// returned, so that no secret or row value reaches the screen.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/exarth-labs/stuntdb/internal/buildinfo"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/redact"
)

// globalOptions holds the flags that every command accepts, and what all
// commands share.
type globalOptions struct {
	json bool

	// redactor removes secrets and row values from errors and logs.
	// Commands register the secrets they handle, such as connection URLs.
	redactor *redact.Redactor
}

// Execute runs stuntdb with args. The returned error carries an errs.Kind,
// and its message is already redacted.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts := &globalOptions{redactor: redact.New()}
	root := newRootCmd(stdout, stderr, opts)
	root.SetArgs(args)
	return reportError(stderr, opts.redactor, root.ExecuteContext(ctx))
}

// reportError gives err a kind if it has none, redacts it, and prints it.
func reportError(stderr io.Writer, redactor *redact.Redactor, err error) error {
	if err == nil {
		return nil
	}
	// Errors from our own commands always carry a kind (see runE). Anything
	// else comes from cobra itself: an unknown command, flag or argument.
	if _, ok := errors.AsType[*errs.Error](err); !ok {
		err = errs.E(errs.Usage, err)
	}
	err = redactor.Error(err)
	fmt.Fprintf(stderr, "stuntdb: %v\n", err)
	if errs.KindOf(err) == errs.Usage {
		fmt.Fprintln(stderr, "Run 'stuntdb --help' for usage.")
	}
	return err
}

func newRootCmd(stdout, stderr io.Writer, opts *globalOptions) *cobra.Command {
	root := &cobra.Command{
		Use:   "stuntdb",
		Short: "Make small, safe, realistic copies of production databases",
		Long: "stuntdb makes a small, safe copy of a production database: personal data is\n" +
			"replaced with consistent fake values, and every relationship between tables is kept.",
		Version:       buildinfo.Get().Version,
		SilenceErrors: true, // Execute prints errors itself
		SilenceUsage:  true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetVersionTemplate("stuntdb {{.Version}}\n")
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "print results as JSON")

	root.AddCommand(newVersionCmd(opts))
	return root
}

// runE marks every error a command returns with a kind, defaulting to
// errs.Internal, so that Execute can tell them apart from cobra's own errors.
func runE(fn func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := fn(cmd, args)
		if err == nil {
			return nil
		}
		if _, ok := errors.AsType[*errs.Error](err); !ok {
			return errs.E(errs.Internal, err)
		}
		return err
	}
}
