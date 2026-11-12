package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
)

const (
	keepEvery  = 30 * time.Second
	pruneEvery = 10 * time.Minute
)

var (
	reopenWithin = 2 * time.Minute
	answerWithin = time.Minute
	storageGone  = 10 * time.Minute
)

type storageCheck func(ctx context.Context, task *orcaTask) error

var keepCommand = func(config string) (*exec.Cmd, error) {
	helper, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(helper, "orca", "keep", "--config", config), nil
}

func newOrcaKeepCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "keep",
		Short: "Reopen the SSH forward of every kept task whose forward has exited, until no task is kept",
		Long: `keep runs detached, started by orca create, prepare, and reconnect once a task's worker answers. Every
30 seconds it checks the SSH forward of each kept task and reopens one that has exited, as reconnect
does, without touching the runtime or the worker. Each reopen gets two minutes, since reopening the
forward of a cold Sprite waits for it to boot, which took about 67 seconds to its Orca runtime, so a
machine that answers nothing costs one pass two minutes, never the forwards after it. When a reopen
fails, keep asks the provider whether the machine's storage is reachable, and once a failing machine
has reported its storage gone for 10 minutes, keep releases its task, logs that once, and reopens it
no more; orca reconnect keeps it again. orca stop and destroy release a task before they close its forward, so keep
never reopens a stopped lane's forward. Every 10 minutes, starting with its first pass, it also runs
orca prune, so a stopped lane's or a gone machine's Orca environment does not linger for Orca to keep
reading and retrying. When orca.keys.github_app names a command, each pass runs it and writes a line it
has not yet written to a kept task's machine into that machine's GitHub App token file, so a worker's
ccx keeps reading on the app's quota past the hour its launch token lasts. One keep runs per state
directory: a keep that finds another running exits at once, and a keep exits once no task is kept.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			prune := func(ctx context.Context) error {
				session, err := flags.open()
				if err != nil {
					return err
				}
				_, err = pruneEnvironments(ctx, session, orca.NewClient(runner), false)
				return err
			}
			storage := func(ctx context.Context, task *orcaTask) error {
				provider, err := openProvider(cfg, task.Provider)
				if err != nil {
					return err
				}
				checker, ok := provider.(providers.StorageChecker)
				if !ok {
					return nil
				}
				return checker.CheckStorage(ctx, task.Machine)
			}
			tokens := appTokens{command: cfg.Orca.Keys[config.KeyGitHubApp], sent: map[string][]byte{}}
			return keepForwards(cmd.Context(), cfg.State(), slog.Default(), prune, storage, tokens)
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

func keepForwards(ctx context.Context, dir state.Dir, log *slog.Logger, prune func(context.Context) error, storage storageCheck, tokens appTokens) error {
	for {
		unlock, held, err := state.TryLock(dir.OrcaKeeper())
		if err != nil || !held {
			return err
		}
		err = holdForwards(ctx, dir, log, prune, storage, tokens)
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

func holdForwards(ctx context.Context, dir state.Dir, log *slog.Logger, prune func(context.Context) error, storage storageCheck, tokens appTokens) error {
	ticker := time.NewTicker(keepEvery)
	defer ticker.Stop()
	var pruned time.Time
	gone := map[string]time.Time{}
	for {
		if time.Since(pruned) >= pruneEvery {
			if err := prune(ctx); err != nil {
				log.Warn("pruning Orca environments failed, and the next prune tries again", "error", err)
			}
			pruned = time.Now()
		}
		kept, err := reopenForwards(ctx, dir, log, storage, gone)
		if err != nil || kept == 0 {
			return err
		}
		if err := tokens.renew(ctx, dir, log); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func reopenForwards(ctx context.Context, dir state.Dir, log *slog.Logger, storage storageCheck, gone map[string]time.Time) (int, error) {
	names, err := keptNames(dir)
	if err != nil {
		return 0, err
	}
	kept := len(names)
	still := map[string]time.Time{}
	defer func() {
		clear(gone)
		maps.Copy(gone, still)
	}()
	for _, name := range names {
		task, err := loadTask(dir, name)
		if err != nil {
			return 0, err
		}
		reopened, err := reopenForward(ctx, dir, task)
		if err == nil {
			if reopened {
				log.Info("reopened a kept forward", "workspace", name)
			}
			continue
		}
		if !storageLost(ctx, storage, task) {
			log.Warn("a kept forward did not reopen, and the next pass tries again", "workspace", name, "error", err)
			continue
		}
		since, seen := gone[task.Machine]
		if !seen {
			still[task.Machine] = time.Now()
			continue
		}
		if time.Since(since) < storageGone {
			still[task.Machine] = since
			continue
		}
		if err := state.Remove(dir.OrcaKept(name)); err != nil {
			return 0, err
		}
		kept--
		log.Warn("released a kept forward whose machine has reported its storage gone since the first failed reopen, so keep reopens it no more; orca reconnect keeps it again", "workspace", name, "machine", task.Machine, "since", since, "error", err)
	}
	return kept, nil
}

func reopenForward(ctx context.Context, dir state.Dir, task *orcaTask) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, reopenWithin)
	defer cancel()
	if task.Forward.up(ctx) {
		return false, nil
	}
	if _, err := os.Stat(dir.OrcaKept(task.Workspace)); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err := task.Forward.ensure(ctx, task.Port); err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("the reopen did not finish within %s: %w", reopenWithin, err)
		}
		return false, err
	}
	return true, nil
}

type appTokens struct {
	command []string
	sent    map[string][]byte
}

func (a appTokens) renew(ctx context.Context, dir state.Dir, log *slog.Logger) error {
	if len(a.command) == 0 {
		return nil
	}
	line, err := captureKey(ctx, a.command)
	if err != nil {
		log.Warn("the orca.keys.github_app command failed, so each worker reads through the GitHub App token it holds until a later pass renews it", "error", err)
		return nil
	}
	names, err := keptNames(dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		if bytes.Equal(a.sent[name], line) {
			continue
		}
		task, err := loadTask(dir, name)
		if err != nil {
			return err
		}
		if err := sendAppToken(ctx, task, line); err != nil {
			log.Warn("a kept task's GitHub App token did not renew, and the next pass tries again", "workspace", name, "error", err)
			continue
		}
		a.sent[name] = line
	}
	return nil
}

func sendAppToken(ctx context.Context, task *orcaTask, line []byte) error {
	ctx, cancel := context.WithTimeout(ctx, answerWithin)
	defer cancel()
	if !task.Forward.up(ctx) {
		return errors.New("its forward is down")
	}
	_, err := task.run(ctx, orca.GitHubAppScript, bytes.NewReader(line))
	return err
}

func storageLost(ctx context.Context, storage storageCheck, task *orcaTask) bool {
	ctx, cancel := context.WithTimeout(ctx, answerWithin)
	defer cancel()
	return errors.Is(storage(ctx, task), providers.ErrNoStorage)
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
