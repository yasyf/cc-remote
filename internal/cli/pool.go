package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/workspace"
)

type poolClaim struct {
	Workspace  string         `json:"workspace"`
	Machine    string         `json:"machine"`
	Allocation string         `json:"allocation"`
	Replenish  *orcaReplenish `json:"replenish"`
}

func newPoolCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pool", Short: "Hand out the warm pool's Sprite workspaces, with no Orca task or worker"}
	cmd.AddCommand(newPoolClaimCmd())
	return cmd
}

func newPoolClaimCmd() *cobra.Command {
	var flags selection
	var ref string
	cmd := &cobra.Command{
		Use:   "claim <lane>",
		Short: "Claim an unused warm workspace for a lane, or create one under the lane's name, and print it as JSON",
		Long: `claim takes the first unused warm workspace of the selected Sprite provider and profile for the lane, as
orca prepare --warm does, but starts no Orca runtime, task or worker. With none ready it creates a workspace
named after the lane. Either way it starts a detached orca pool fill to top the pool back up, and prints the
workspace, its machine, and whether it was warm or created. The caller runs its commands with step and ends
with destroy, after which the next fill reaps the record.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			if err := warmable(session); err != nil {
				return err
			}
			lane := args[0]
			if ref == "" {
				ref = session.Config.Ref
			}
			pool := openPool(session)
			if err := pool.admit(lane); err != nil {
				return err
			}
			member, release, err := pool.claim(cmd.Context(), lane, ref)
			if err != nil {
				return err
			}
			claimed := poolClaim{Replenish: pool.replenish()}
			if member != nil {
				defer release()
				record, err := session.Recorded(member.Name)
				if err != nil {
					return err
				}
				claimed.Workspace, claimed.Machine, claimed.Allocation = member.Name, record.Machine, allocationWarm
				return emit(cmd.OutOrStdout(), claimed)
			}
			result, err := session.Create(cmd.Context(), lane, workspace.Source{Ref: ref})
			if err != nil {
				return err
			}
			claimed.Workspace, claimed.Machine, claimed.Allocation = result.Name, result.Machine, allocationCreated
			return emit(cmd.OutOrStdout(), claimed)
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&ref, "ref", "", "branch or tag the workspace checks out (default config.ref)")
	return cmd
}
