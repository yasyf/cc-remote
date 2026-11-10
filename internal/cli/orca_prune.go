package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const (
	prunedStopped = "its lane was stopped"
	prunedGone    = "its machine is gone"
)

type orcaPruned struct {
	DryRun    bool                `json:"dryRun,omitempty"`
	Forgotten []prunedEnvironment `json:"forgotten"`
}

type prunedEnvironment struct {
	Workspace   string `json:"workspace"`
	Environment string `json:"environment"`
	Reason      string `json:"reason"`
}

func newOrcaPruneCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove the Orca environment of every task whose lane was stopped or whose machine is gone",
		Long: `prune removes each saved Orca environment that a task record on this provider paired, once that
task's lane was stopped or the provider no longer has its machine. Orca rereads every saved
environment on each lookup and retries one that does not answer, so a stopped or deleted
workspace's environment costs Orca work until it is removed. An environment matches a task only by
the id the task recorded, so a later pairing under the same name is kept, and an environment no
task paired is never touched. A task on a gone machine is also released, so orca keep no longer
reopens its forward. Its records stay. --dry-run names what prune would remove.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			pruned, err := pruneEnvironments(cmd.Context(), session, orca.NewClient(runner), dryRun)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), pruned)
		},
	}
	flags.bind(cmd)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "name the environments prune would remove without removing them")
	return cmd
}

func pruneEnvironments(ctx context.Context, session *workspace.Session, client orca.Client, dryRun bool) (*orcaPruned, error) {
	dir := session.Config.State()
	tasks, err := recordedTasks(dir)
	if err != nil {
		return nil, err
	}
	saved, err := client.Environments(ctx)
	if err != nil {
		return nil, err
	}
	machines, err := session.Provider.List(ctx, nil)
	if err != nil {
		return nil, err
	}
	alive := make(map[string]bool, len(machines))
	for _, machine := range machines {
		alive[machine.ID] = true
	}
	paired := make(map[string]*orcaTask, len(tasks))
	for _, task := range tasks {
		if task.Provider == session.Kind && task.EnvironmentID != "" {
			paired[task.EnvironmentID] = task
		}
	}
	pruned := &orcaPruned{DryRun: dryRun, Forgotten: []prunedEnvironment{}}
	for _, environment := range saved {
		task, ok := paired[environment.ID]
		if !ok || task.Environment != environment.Name {
			continue
		}
		reason := ""
		switch {
		case task.Stopped:
			reason = prunedStopped
		case !alive[task.Machine]:
			reason = prunedGone
		default:
			continue
		}
		pruned.Forgotten = append(pruned.Forgotten, prunedEnvironment{Workspace: task.Workspace, Environment: environment.ID, Reason: reason})
		if dryRun {
			continue
		}
		if reason == prunedGone {
			if err := state.Remove(dir.OrcaKept(task.Workspace)); err != nil {
				return nil, err
			}
		}
		if err := client.RemoveEnvironment(ctx, environment.ID); err != nil {
			return nil, err
		}
	}
	return pruned, nil
}

func recordedTasks(dir state.Dir) ([]*orcaTask, error) {
	entries, err := os.ReadDir(filepath.Dir(dir.Orca("x")))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tasks []*orcaTask
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || entry.IsDir() || state.ValidateName(name) != nil {
			continue
		}
		task, err := loadTask(dir, name)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}
