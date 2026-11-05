package cli

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/state"
)

const keepEvery = 30 * time.Second

var keepCommand = func(config string) (*exec.Cmd, error) {
	helper, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(helper, "orca", "keep", "--config", config), nil
}

func newOrcaKeepCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "keep",
		Short: "Reopen the SSH forward of every kept task whose forward has exited, until no task is kept",
		Long: `keep runs detached, started by orca create, prepare, and reconnect once a task's worker answers. Every
30 seconds it checks the SSH forward of each kept task and reopens one that has exited, as reconnect
does, without touching the runtime or the worker. orca stop and destroy release a task before they
close its forward, so keep never reopens a stopped lane's forward. One keep runs per state directory:
a keep that finds another running exits at once, and a keep exits once no task is kept.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			return keepForwards(cmd.Context(), cfg.State(), slog.Default())
		},
	}
	flags.bind(cmd)
	return cmd
}

func (d orcaDriver) keep(task *orcaTask, config string) error {
	if task.Forward == nil {
		return nil
	}
	if err := state.Write(d.state.OrcaKept(task.Workspace), nil); err != nil {
		return err
	}
	if err := spawnKeeper(d.state, config); err != nil {
		d.log.Warn("the forward keeper did not start, so a forward that exits stays down until the next launch or reconnect", "workspace", task.Workspace, "error", err)
	}
	return nil
}

func spawnKeeper(dir state.Dir, config string) error {
	cmd, err := keepCommand(config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir.OrcaKeeperLog()), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(dir.OrcaKeeperLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = output.Close() }()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func keepForwards(ctx context.Context, dir state.Dir, log *slog.Logger) error {
	for {
		unlock, held, err := state.TryLock(dir.OrcaKeeper())
		if err != nil || !held {
			return err
		}
		err = holdForwards(ctx, dir, log)
		unlock()
		if err != nil {
			return err
		}
		kept, err := keptNames(dir)
		if err != nil || len(kept) == 0 {
			return err
		}
	}
}

func holdForwards(ctx context.Context, dir state.Dir, log *slog.Logger) error {
	ticker := time.NewTicker(keepEvery)
	defer ticker.Stop()
	for {
		kept, err := reopenForwards(ctx, dir, log)
		if err != nil || kept == 0 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func reopenForwards(ctx context.Context, dir state.Dir, log *slog.Logger) (int, error) {
	names, err := keptNames(dir)
	if err != nil {
		return 0, err
	}
	for _, name := range names {
		task, err := loadTask(dir, name)
		if err != nil {
			return 0, err
		}
		if task.Forward.up(ctx) {
			continue
		}
		if _, err := os.Stat(dir.OrcaKept(name)); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := task.Forward.ensure(ctx, task.Port); err != nil {
			log.Warn("a kept forward did not reopen, and the next pass tries again", "workspace", name, "error", err)
			continue
		}
		log.Info("reopened a kept forward", "workspace", name)
	}
	return len(names), nil
}

func keptNames(dir state.Dir) ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(dir.OrcaKept("x")))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if state.ValidateName(entry.Name()) == nil {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}
