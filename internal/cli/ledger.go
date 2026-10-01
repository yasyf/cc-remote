package cli

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/budget"
)

func (f *selection) store() (budget.Store, error) {
	cfg, err := f.load()
	if err != nil {
		return budget.Store{}, err
	}
	return budget.Store{Path: cfg.State().Ledger(cfg.Budget.Ledger)}, nil
}

func newLedgerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ledger",
		Short: "Start or carry over the budget ledger every create and prepare is admitted against",
		Long: `Nothing is admitted against a ledger that does not exist: cc-remote never
starts one on its own, so spend from an earlier tool cannot vanish behind a
fresh file. ledger init starts an empty one; ledger import copies an existing
ledger of the same format in, recording where it came from.`,
	}
	cmd.AddCommand(newLedgerInitCmd(), newLedgerImportCmd())
	return cmd
}

func newLedgerInitCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Start an empty ledger",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := flags.store()
			if err != nil {
				return err
			}
			if err := store.Init(time.Now()); err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), map[string]string{"ledger": store.Path, "origin": budget.Created})
		},
	}
	flags.bind(cmd)
	return cmd
}

func newLedgerImportCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "import <path>",
		Short: "Carry an existing ledger over, keeping its history and provenance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := flags.store()
			if err != nil {
				return err
			}
			ledger, err := store.Import(args[0], time.Now())
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), map[string]any{"ledger": store.Path, "origin": ledger.Origin, "resources": len(ledger.Resources)})
		},
	}
	flags.bind(cmd)
	return cmd
}
