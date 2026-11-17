package cli

import (
	"context"
	"errors"

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
orca prepare --warm does, checks out the requested ref on it while it holds the claim, and starts no Orca
runtime, task or worker. With none ready it creates a workspace
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
			if ref == "" {
				ref = session.Config.Ref
			}
			claimed, err := claimWorkspace(cmd.Context(), session, args[0], ref)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), claimed)
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&ref, "ref", "", "branch or tag the workspace checks out (default config.ref)")
	return cmd
}

func claimWorkspace(ctx context.Context, session *workspace.Session, lane, ref string) (poolClaim, error) {
	pool := openPool(session)
	if err := pool.admit(lane); err != nil {
		return poolClaim{}, err
	}
	member, release, err := pool.claim(ctx, lane, ref)
	if err != nil {
		return poolClaim{}, err
	}
	claimed := poolClaim{Replenish: pool.replenish()}
	if member != nil {
		defer release()
		result, err := session.Reuse(ctx, member.Name, workspace.Source{Ref: ref})
		if err := errors.Join(err, pool.settle(member, nil, err)); err != nil {
			return poolClaim{}, err
		}
		claimed.Workspace, claimed.Machine, claimed.Allocation = member.Name, result.Machine, allocationWarm
		return claimed, nil
	}
	result, err := session.Create(ctx, lane, workspace.Source{Ref: ref})
	if err != nil {
		return poolClaim{}, err
	}
	claimed.Workspace, claimed.Machine, claimed.Allocation = result.Name, result.Machine, allocationCreated
	return claimed, nil
}
