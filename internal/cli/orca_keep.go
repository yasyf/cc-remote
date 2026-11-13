package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/version"
)

const (
	keepEvery   = 30 * time.Second
	pruneEvery  = 10 * time.Minute
	renewBefore = time.Hour
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

var execKeeper = func(next successor) error {
	return syscall.Exec(next.Binary, []string{next.Binary, "orca", "keep", "--config", next.Config}, os.Environ())
}

func newOrcaKeepCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "keep",
		Short: "Hold the forward of every kept Sprite task and reopen any other kept task's SSH forward that has exited, until no task is kept",
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
reading and retrying. Each pass reads again the config that kept each task, the one its create,
prepare or reconnect named, so a key added to that config reaches the task with no restart of keep.
When a task's config names an orca.keys.github_app command, the pass runs it and writes a line it has
not yet written to that task's machine into its GitHub App token file. A launch never runs that
command, so this write hands a worker its first token as well as each renewal, and a failed mint on
the Mac never fails a launch. When a task's config names an orca.keys.aws
command, a pass runs it once the session it last printed has under an hour left and writes the new
session into the worker's AWS credentials file the same way, so a worker's AWS reads outlive the
session its launch captured. Tasks whose configs name the same command share what it prints. One keep runs per state directory: a keep that finds another running
records itself as that keep's successor and exits, the running keep execs a successor of a newer
release at its next pass, so a pin change reaches the keep, and a keep exits once no task is kept.

A kept Sprite task has no SSH forward: keep itself listens on the task's recorded 127.0.0.1 port and
bridges each connection to the same port on the Sprite over the Sprites API's port proxy, one process
for every kept Sprite task. Every second it holds a newly kept Sprite task's port and closes a released
one's port and connections, so orca stop and destroy close a lane's forward at once. A Sprite task
recorded with an SSH forward keeps that forward while its master answers, since a prepare of an
earlier release runs its commands through it, and once the master exits keep holds the same port
the same way, so Orca's saved environment keeps the port it names. A newer keep taking over drops
these connections for the moment between the two processes, and Orca connects again.`,
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
			open := func(task *orcaTask) (providers.Provider, error) {
				kept, err := os.ReadFile(cfg.State().OrcaKept(task.Workspace))
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, err
				}
				source := cfg
				if len(kept) > 0 {
					if source, err = config.Load(string(kept)); err != nil {
						return nil, err
					}
				}
				return openProvider(source, task.Provider)
			}
			storage := func(ctx context.Context, task *orcaTask) error {
				provider, err := open(task)
				if err != nil {
					return err
				}
				checker, ok := provider.(providers.StorageChecker)
				if !ok {
					return nil
				}
				return checker.CheckStorage(ctx, task.Machine)
			}
			binary, err := os.Executable()
			if err != nil {
				return err
			}
			renewals := []*renewal{
				newRenewal(config.KeyGitHubApp, captureAppToken, orca.GitHubAppScript),
				newRenewal(config.KeyAWS, captureAWS, orca.AWSScript),
			}
			self := successor{Version: version.String(), Binary: binary, Config: cfg.Path}
			return keepForwards(cmd.Context(), cfg.State(), slog.Default(), prune, storage, open, renewals, self)
		},
	}
	flags.bind(cmd)
	return cmd
}

func (d orcaDriver) keep(task *orcaTask, config string) error {
	if task.Forward == nil && !task.proxied() {
		return nil
	}
	if err := state.Write(d.state.OrcaKept(task.Workspace), []byte(config)); err != nil {
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

func keepForwards(ctx context.Context, dir state.Dir, log *slog.Logger, prune func(context.Context) error, storage storageCheck, open taskProvider, renewals []*renewal, self successor) error {
	for {
		unlock, held, err := state.TryLock(dir.OrcaKeeper())
		if err != nil {
			return err
		}
		if !held {
			return offerSuccessor(dir, self)
		}
		next, err := holdForwards(ctx, dir, log, prune, storage, open, renewals, self.Version)
		unlock()
		if err != nil {
			return err
		}
		if next != nil {
			log.Info("handing the keeper to a newer release", "version", next.Version, "binary", next.Binary)
			err := execKeeper(*next)
			log.Warn("the newer keeper did not start, so this one keeps holding", "version", next.Version, "binary", next.Binary, "error", err)
			if err := os.Remove(dir.OrcaKeeperSuccessor()); err != nil {
				return err
			}
			continue
		}
		kept, err := keptNames(dir)
		if err != nil || len(kept) == 0 {
			return err
		}
	}
}

func holdForwards(ctx context.Context, dir state.Dir, log *slog.Logger, prune func(context.Context) error, storage storageCheck, open taskProvider, renewals []*renewal, running string) (*successor, error) {
	ticker := time.NewTicker(keepEvery)
	defer ticker.Stop()
	held := newProxies(dir, log, open)
	holding, stop := context.WithCancel(ctx)
	var proxying sync.WaitGroup
	proxying.Go(func() { held.run(holding) })
	defer func() {
		stop()
		proxying.Wait()
	}()
	var pruned time.Time
	gone := map[string]time.Time{}
	for {
		next, err := readSuccessor(dir)
		if err != nil {
			return nil, err
		}
		if next != nil && newerRelease(next.Version, running) {
			return next, nil
		}
		if time.Since(pruned) >= pruneEvery {
			if err := prune(ctx); err != nil {
				log.Warn("pruning Orca environments failed, and the next prune tries again", "error", err)
			}
			pruned = time.Now()
		}
		kept, err := reopenForwards(ctx, dir, log, storage, held, gone)
		if err != nil || kept == 0 {
			return nil, err
		}
		keys, err := keptKeys(dir, log)
		if err != nil {
			return nil, err
		}
		for _, r := range renewals {
			if err := r.renew(ctx, dir, log, open, keys); err != nil {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

type successor struct {
	Version string `json:"version"`
	Binary  string `json:"binary"`
	Config  string `json:"config"`
}

func readSuccessor(dir state.Dir) (*successor, error) {
	raw, err := os.ReadFile(dir.OrcaKeeperSuccessor())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var next successor
	return &next, json.Unmarshal(raw, &next)
}

func offerSuccessor(dir state.Dir, self successor) error {
	if releaseParts(self.Version) == nil {
		return nil
	}
	offered, err := readSuccessor(dir)
	if err != nil || (offered != nil && !newerRelease(self.Version, offered.Version)) {
		return err
	}
	return state.Save(dir.OrcaKeeperSuccessor(), self)
}

func newerRelease(candidate, running string) bool {
	a, b := releaseParts(candidate), releaseParts(running)
	return a != nil && b != nil && slices.Compare(a, b) > 0
}

func releaseParts(v string) []int {
	fields := strings.Split(v, ".")
	if len(fields) != 3 {
		return nil
	}
	parts := make([]int, 3)
	for i, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || strconv.Itoa(n) != field || n < 0 {
			return nil
		}
		parts[i] = n
	}
	return parts
}

func reopenForwards(ctx context.Context, dir state.Dir, log *slog.Logger, storage storageCheck, held *proxies, gone map[string]time.Time) (int, error) {
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
		reopened, err := reopenForward(ctx, dir, held, task)
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

func reopenForward(ctx context.Context, dir state.Dir, held *proxies, task *orcaTask) (bool, error) {
	if task.proxied() {
		return false, held.failure(task.Workspace)
	}
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

type renewal struct {
	key     string
	capture func(context.Context, state.Dir, []string) ([]byte, time.Time, error)
	script  string
	minted  map[string]minted
	sent    map[string][]byte
}

type minted struct {
	line    []byte
	expires time.Time
}

func newRenewal(key string, capture func(context.Context, state.Dir, []string) ([]byte, time.Time, error), script string) *renewal {
	return &renewal{key: key, capture: capture, script: script, minted: map[string]minted{}, sent: map[string][]byte{}}
}

func captureAppToken(ctx context.Context, dir state.Dir, command []string) ([]byte, time.Time, error) {
	line, err := captureKey(ctx, dir, command)
	return line, time.Time{}, err
}

func keptKeys(dir state.Dir, log *slog.Logger) (map[string]map[string][]string, error) {
	names, err := keptNames(dir)
	if err != nil {
		return nil, err
	}
	configs := map[string]*config.Config{}
	keys := map[string]map[string][]string{}
	for _, name := range names {
		path, err := os.ReadFile(dir.OrcaKept(name))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return nil, err
		case len(path) == 0:
			continue
		}
		cfg, read := configs[string(path)]
		if !read {
			if cfg, err = config.Load(string(path)); err != nil {
				log.Warn("the config that kept a task did not load, so keep renews none of the keys of the tasks it kept until a later pass loads it", "config", string(path), "error", err)
			}
			configs[string(path)] = cfg
		}
		if cfg != nil {
			keys[name] = cfg.Orca.Keys
		}
	}
	return keys, nil
}

func (r *renewal) renew(ctx context.Context, dir state.Dir, log *slog.Logger, open taskProvider, keys map[string]map[string][]string) error {
	lines := map[string][]byte{}
	for _, name := range slices.Sorted(maps.Keys(keys)) {
		command := keys[name][r.key]
		if len(command) == 0 {
			continue
		}
		id := fmt.Sprintf("%q", command)
		line, tried := lines[id]
		if !tried {
			line = r.mint(ctx, dir, log, id, command)
			lines[id] = line
		}
		if line == nil || bytes.Equal(r.sent[name], line) {
			continue
		}
		task, err := loadTask(dir, name)
		if err != nil {
			return err
		}
		if err := r.send(ctx, task, open, line); err != nil {
			log.Warn("a kept task's renewed key did not reach it, and the next pass tries again", "key", r.key, "workspace", name, "error", err)
			continue
		}
		r.sent[name] = line
	}
	return nil
}

func (r *renewal) mint(ctx context.Context, dir state.Dir, log *slog.Logger, id string, command []string) []byte {
	if held := r.minted[id]; time.Until(held.expires) >= renewBefore {
		return held.line
	}
	line, expires, err := r.capture(ctx, dir, command)
	if err != nil {
		log.Warn("an orca.keys command keep renews failed, so each worker reads through what it holds until a later pass renews it", "key", r.key, "error", err)
		return nil
	}
	r.minted[id] = minted{line: line, expires: expires}
	return line
}

func (r *renewal) send(ctx context.Context, task *orcaTask, open taskProvider, line []byte) error {
	ctx, cancel := context.WithTimeout(ctx, answerWithin)
	defer cancel()
	switch {
	case task.proxied():
		provider, err := open(task)
		if err != nil {
			return err
		}
		task.provider = provider
	case !task.Forward.up(ctx):
		return errors.New("its forward is down")
	}
	_, err := task.run(ctx, r.script, bytes.NewReader(line))
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
