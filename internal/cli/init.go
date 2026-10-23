package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/exarth-labs/stuntdb/internal/app"
)

func newInitCmd(opts *globalOptions) *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter stuntdb.yaml, with a suggested masking rule for each column",
		Long: "init connects to the source database, reads its structure (never its rows), and\n" +
			"writes a starter stuntdb.yaml. Each masking rule it suggests is written as a\n" +
			"comment, so that none is used until you review it. init never replaces a file.\n\n" +
			"The source URL comes from --source, the STUNTDB_SOURCE_URL environment variable,\n" +
			"or your own defaults. A URL with a password must come from the environment.",
		Args: cobra.NoArgs,
		RunE: runE(func(cmd *cobra.Command, _ []string) error {
			res, err := app.Init(cmd.Context(), app.InitRequest{Env: opts.env, ConfigPath: opts.config, SourceURL: source})
			if errors.Is(err, app.ErrNoSource) {
				return fmt.Errorf("%w, or use --source", err)
			}
			if err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), opts.json, res, initText)
		}),
	}
	cmd.Flags().StringVar(&source, "source", "", "the source database URL, such as sqlite:./app.db (without a password)")
	return cmd
}

func initText(r *app.InitResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Wrote %s for %s in %s:\n", r.Path, plural(r.Columns, "column"), plural(r.Tables, "table"))
	writeCounts(&b, r.Columns, []count{
		{r.Suggested, "with a suggested rule to review"},
		{r.NoSuggestion, "without a suggestion"},
		{r.NoRuleNeeded, "that need no rule"},
	})
	fmt.Fprintf(&b, "\nNext: review the rules in %s, then run \"stuntdb check --update\".\n", r.Path)
	return b.String()
}
