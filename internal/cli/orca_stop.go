package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

type orcaStopped struct {
	Lane      string `json:"lane"`
	Workspace string `json:"workspace"`
	Found     string `json:"found,omitempty"`
}

func newOrcaStopCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "stop <lane>",
		Short: "Close a lane's forward, remove its Orca environment, and suspend the workspace it runs on",
		Long: `stop finds the workspace a lane runs on, the one prepare created under the lane's name or the
warm pool workspace the lane claimed, releases the task so orca keep no longer reopens its forward,
closes that SSH forward, removes the Orca environment paired with it, and suspends the machine. A suspended machine's runtime never answers that pairing
again, and Orca retries a saved environment that does not answer without end. Its disk, workspace
record, task record, and pool record stay, and the pool record turns stopped, so prepare --warm
admits the lane again and claims it a fresh workspace. A warm pool workspace's own name is refused,
so stop reaches only a workspace a lane claimed. A claim that failed before its worker started, or
one already stopped, is not where the lane runs, so stop passes over it.

A lane with no task record has no forward to close. A lane with no workspace record, or whose
machine the provider no longer has, is already stopped: stop removes the records left for it, as
destroy would, names what it found under "found", and succeeds. A machine whose storage the
provider cannot reach runs nothing again, so it is stopped too: stop names that under
"found" and succeeds, and destroys nothing, so its records stay.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			stopped, err := stopLane(cmd.Context(), session, orca.NewClient(runner), args[0])
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), stopped)
		},
	}
	flags.bind(cmd)
	return cmd
}

func stopLane(ctx context.Context, session *workspace.Session, client orca.Client, lane string) (*orcaStopped, error) {
	pool := openPool(session)
	name, err := pool.workspaceOf(lane)
	if err != nil {
		return nil, err
	}
	dir := session.Config.State()
	if err := disconnect(ctx, dir, client, name); err != nil {
		return nil, err
	}
	stopped := &orcaStopped{Lane: lane, Workspace: name}
	err = session.Suspend(ctx, name)
	if err == nil {
		return stopped, pool.stop(name)
	}
	if errors.Is(err, providers.ErrNoStorage) {
		stopped.Found = err.Error()
		return stopped, pool.stop(name)
	}
	if !errors.Is(err, workspace.ErrNotRecorded) && !errors.Is(err, providers.ErrNotFound) {
		return nil, err
	}
	stopped.Found = err.Error()
	if err := session.Destroy(ctx, name); err != nil && !errors.Is(err, workspace.ErrNotRecorded) {
		return nil, err
	}
	if err := state.Remove(dir.Orca(name)); err != nil {
		return nil, err
	}
	return stopped, nil
}

func disconnect(ctx context.Context, dir state.Dir, client orca.Client, name string) error {
	if err := state.Remove(dir.OrcaKept(name)); err != nil {
		return err
	}
	task, err := loadTask(dir, name)
	switch {
	case errors.Is(err, errNoTask):
		return nil
	case err != nil:
		return err
	case task.Forward != nil:
		if err := task.Forward.close(ctx); err != nil {
			return err
		}
	}
	return client.ForgetEnvironment(ctx, task.Environment)
}

func (p *orcaPool) workspaceOf(lane string) (string, error) {
	records, err := p.records()
	if err != nil {
		return "", err
	}
	for _, member := range records {
		if member.Name == lane {
			return "", fmt.Errorf("%s names a warm pool workspace, not a lane; a workspace is stopped only through the lane that claimed it", lane)
		}
		if member.Lane != lane || member.State == memberStopped {
			continue
		}
		started, err := p.startedWorker(member)
		if err != nil {
			return "", err
		}
		if started {
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
