// Package cli implements stuntdb's command-line interface. It parses flags,
// calls the application layer and renders results; it holds no business logic.
//
// Results go to stdout and messages go to stderr, so that stdout stays clean
// for scripts and pipes. Every error is redacted before it is printed or
// returned, so that no secret or row value reaches the screen.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/exarth-labs/stuntdb/internal/app"
	"github.com/exarth-labs/stuntdb/internal/buildinfo"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/redact"
)

// globalOptions holds the flags that every command accepts, and what all
// commands share.
type globalOptions struct {
	json   bool
	config string // the project configuration file

	// redactor removes secrets and row values from errors and logs.
	// Commands register the secrets they handle, such as connection URLs.
	redactor *redact.Redactor

	// env is what the use cases need from outside; its Redactor is redactor.
	env app.Env
}

// Execute runs stuntdb with args. The returned error carries an errs.Kind,
// and its message is already redacted.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return execute(ctx, args, stdout, stderr, app.Env{})
}

// execute runs stuntdb with the given environment, which tests use to
// supply their own database adapters.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer, env app.Env) error {
	opts := &globalOptions{redactor: redact.New(), env: env}
	opts.env.Redactor = opts.redactor
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
	root.PersistentFlags().StringVarP(&opts.config, "config", "c", config.DefaultPath, "the project configuration file; stuntdb.lock is next to it")

	root.AddCommand(newInitCmd(opts), newCheckCmd(opts), newVersionCmd(opts))
	return root
}

// render writes a command's result to w: as indented JSON with --json, and
// otherwise as the text that text writes.
func render[T any](w io.Writer, asJSON bool, result T, text func(T) string) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	_, err := io.WriteString(w, text(result))
	return err
}

// plural writes a count and a noun, such as "1 column" or "3 columns".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
