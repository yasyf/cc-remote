package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/workspace"
)

type orcaStopped struct {
	Lane      string `json:"lane"`
	Workspace string `json:"workspace"`
}

func newOrcaStopCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "stop <lane>",
		Short: "Close a lane's forward and suspend the workspace it runs on",
		Long: `stop finds the workspace a lane runs on, the one prepare created under the lane's name or the
warm pool workspace the lane claimed, closes the task's SSH forward, and suspends the machine. Its
disk, workspace record, task record, and pool record stay. A warm pool workspace's own name is
refused, so stop reaches only a workspace a lane claimed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			stopped, err := stopLane(cmd.Context(), session, args[0])
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), stopped)
		},
	}
	flags.bind(cmd)
	return cmd
}

func stopLane(ctx context.Context, session *workspace.Session, lane string) (*orcaStopped, error) {
	name, err := openPool(session).workspaceOf(lane)
	if err != nil {
		return nil, err
	}
	task, err := loadTask(session.Config.State(), name)
	if err != nil {
		return nil, err
	}
	if task.Forward != nil {
		if err := task.Forward.close(ctx); err != nil {
			return nil, err
		}
	}
	if err := session.Suspend(ctx, name); err != nil {
		return nil, err
	}
	return &orcaStopped{Lane: lane, Workspace: name}, nil
}

func (p *orcaPool) workspaceOf(lane string) (string, error) {
	records, err := p.records()
	if err != nil {
		return "", err
	}
	for _, member := range records {
		switch {
		case member.Name == lane:
			return "", fmt.Errorf("%s names a warm pool workspace, not a lane; a workspace is stopped only through the lane that claimed it", lane)
		case member.Lane == lane:
			return member.Name, nil
		}
	}
	return lane, nil
}

func (t orcaTunnel) close(ctx context.Context) error {
	if !t.up(ctx) {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "ssh", t.args("-O", "exit")...).CombinedOutput(); err != nil {
		return fmt.Errorf("close the forward to %s: %w: %s", t.Host, err, bytes.TrimSpace(out))
	}
	return nil
}
