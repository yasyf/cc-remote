package cli

import (
	"context"
	"errors"
	"fmt"
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
	reapDestroys   = 10
	releasedReason = "orca release ended its lane"
)

var errLaunchHolds = errors.New("a launch holds it")

type orcaReleased struct {
	Lane       string   `json:"lane"`
	Workspaces []string `json:"workspaces"`
}

type orcaReaped struct {
	Destroyed []string `json:"destroyed,omitempty"`
	Forgotten []string `json:"forgotten,omitempty"`
	Locks     int      `json:"locks,omitempty"`
}

func newOrcaReleaseCmd(runner orca.Runner) *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "release <lane>",
		Short: "Destroy every workspace a lane ran on once the lane has ended, and remove its records",
		Long: `release is the call for a lane that has ended for good. It finds every warm pool workspace the lane
claimed, stopped ones included, and the workspace prepare created under the lane's name, then closes each
one's forward, removes its Orca environment, destroys its machine, and removes its workspace, task, and pool
records. The machine's disk goes with it, uncommitted work included, so a lane that may continue later is
stopped with orca stop instead. Each pool workspace is first recorded released, so one whose destroy fails, or
that a running launch still holds, is destroyed by the next orca pool fill. A warm pool workspace's own name is
refused. stdout names the lane and the workspaces it released.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			released, err := releaseLane(cmd.Context(), session, orca.NewClient(runner), args[0])
			if released == nil {
				return err
			}
			return errors.Join(err, emit(cmd.OutOrStdout(), released))
		},
	}
	flags.bind(cmd)
	return cmd
}

func releaseLane(ctx context.Context, session *workspace.Session, client orca.Client, lane string) (*orcaReleased, error) {
	pool := openPool(session)
	claimed, err := pool.markReleased(lane)
	if err != nil {
		return nil, err
	}
	released := &orcaReleased{Lane: lane, Workspaces: claimed}
	if _, err := session.Recorded(lane); err == nil {
		released.Workspaces = append(released.Workspaces, lane)
	} else if !errors.Is(err, workspace.ErrNotRecorded) {
		return nil, err
	}
	saved, err := pairedEnvironments(ctx, client)
	if err != nil {
		return released, err
	}
	var failed error
	for _, name := range released.Workspaces {
		if err := pool.end(ctx, client, name, saved, true); err != nil {
			failed = errors.Join(failed, fmt.Errorf("release %s: %w", name, err))
		}
	}
	return released, failed
}

func (p *orcaPool) markReleased(lane string) ([]string, error) {
	var claimed []string
	err := p.locked(func() error {
		records, err := p.records()
		if err != nil {
			return err
		}
		for _, member := range records {
			if member.Name == lane {
				return fmt.Errorf("%s names a warm pool workspace, not a lane; a workspace is released only through the lane that claimed it", lane)
			}
			if member.Lane != lane {
				continue
			}
			claimed = append(claimed, member.Name)
			member.State, member.Reason = memberReleased, releasedReason
			if err := p.save(member); err != nil {
				return err
			}
		}
		return nil
	})
	return claimed, err
}

func pairedEnvironments(ctx context.Context, client orca.Client) (map[string]bool, error) {
	environments, err := client.Environments(ctx)
	if err != nil {
		return nil, err
	}
	saved := make(map[string]bool, len(environments))
	for _, environment := range environments {
		saved[environment.ID] = true
	}
	return saved, nil
}

func (p *orcaPool) reap(ctx context.Context, client orca.Client) (*orcaReaped, error) {
	machines, err := p.session.Provider.List(ctx, nil)
	if err != nil {
		return nil, err
	}
	alive := make(map[string]bool, len(machines))
	for _, machine := range machines {
		alive[machine.ID] = true
	}
	saved, err := pairedEnvironments(ctx, client)
	if err != nil {
		return nil, err
	}
	reaped := &orcaReaped{}
	records, err := p.records()
	if err != nil {
		return nil, err
	}
	pooled := map[string]bool{}
	for _, member := range records {
		pooled[member.Name] = true
		if member.Provider != p.session.Kind || member.State == memberProvisioning {
			continue
		}
		live := alive[p.machineOf(member.Name)]
		switch {
		case !live:
			p.reapOne(ctx, client, member.Name, saved, false, &reaped.Forgotten)
		case member.State == memberReleased && len(reaped.Destroyed) < reapDestroys:
			p.reapOne(ctx, client, member.Name, saved, true, &reaped.Destroyed)
		}
	}
	names, err := recordNames(filepath.Dir(p.dir.Workspace("x")))
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if pooled[name] {
			continue
		}
		record, err := p.session.Recorded(name)
		if err != nil {
			p.session.Log.Warn("the reaper could not read a workspace record, so it stays", "workspace", name, "error", err)
			continue
		}
		if record.Provider != p.session.Kind || record.Machine == "" || record.Unverified || alive[record.Machine] {
			continue
		}
		p.reapOne(ctx, client, name, saved, false, &reaped.Forgotten)
	}
	if err := p.reapTasks(ctx, client, saved, reaped); err != nil {
		return reaped, err
	}
	reaped.Locks, err = p.reapLocks()
	return reaped, err
}

func (p *orcaPool) machineOf(name string) string {
	if record, err := p.session.Recorded(name); err == nil && record.Machine != "" {
		return record.Machine
	}
	return name
}

func (p *orcaPool) reapOne(ctx context.Context, client orca.Client, name string, saved map[string]bool, destroy bool, into *[]string) {
	err := p.end(ctx, client, name, saved, destroy)
	switch {
	case errors.Is(err, errLaunchHolds):
	case err != nil:
		p.session.Log.Warn("the reaper left a workspace whose cleanup failed, and the next fill tries again", "workspace", name, "failure", failureOf(err).describe())
	default:
		*into = append(*into, name)
	}
}

func (p *orcaPool) end(ctx context.Context, client orca.Client, name string, saved map[string]bool, destroy bool) error {
	unlock, held, err := state.TryLock(p.dir.Orca(name) + ".lock")
	if err != nil {
		return err
	}
	if !held {
		return errLaunchHolds
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failedStopWithin)
	defer cancel()
	if err := p.unpair(ctx, client, name, saved, destroy); err != nil {
		return err
	}
	if err := p.session.Destroy(ctx, name); err != nil && !errors.Is(err, workspace.ErrNotRecorded) {
		return err
	}
	return p.removeTask(name)
}

func (p *orcaPool) unpair(ctx context.Context, client orca.Client, name string, saved map[string]bool, live bool) error {
	if err := state.Remove(p.dir.OrcaKept(name)); err != nil {
		return err
	}
	task, err := loadTask(p.dir, name)
	switch {
	case errors.Is(err, errNoTask):
		return nil
	case err != nil:
		return err
	case saved[task.EnvironmentID]:
		return forget(ctx, p.dir, client, task, task.EnvironmentID, prunedGone)
	case live && task.proxied():
		return task.release(ctx)
	case live && task.Forward != nil:
		return task.Forward.close(ctx)
	}
	return nil
}

func (p *orcaPool) removeTask(name string) error {
	return errors.Join(
		state.Remove(p.dir.Orca(name)),
		state.Remove(p.dir.OrcaForwardLog(name)),
		removeIdleLock(p.dir.OrcaProxyLock(name)),
		state.Remove(p.dir.Orca(name)+".lock"),
		removeIdleLock(p.dir.Workspace(name)+".lock"),
	)
}

func (p *orcaPool) reapTasks(ctx context.Context, client orca.Client, saved map[string]bool, reaped *orcaReaped) error {
	names, err := recordNames(filepath.Dir(p.dir.Orca("x")))
	if err != nil {
		return err
	}
	for _, name := range names {
		if p.dir.Orca(name) == p.dir.OrcaKeeperSuccessor() || recorded(p.dir.Workspace(name)) || recorded(p.dir.Pool(name)) {
			continue
		}
		task, err := loadTask(p.dir, name)
		if err != nil {
			p.session.Log.Warn("the reaper could not read a task record, so it stays", "workspace", name, "error", err)
			continue
		}
		if task.Provider != p.session.Kind {
			continue
		}
		p.reapOne(ctx, client, name, saved, false, &reaped.Forgotten)
	}
	return nil
}

func (p *orcaPool) reapLocks() (int, error) {
	removed := 0
	for _, dir := range []string{filepath.Dir(p.dir.Orca("x")), filepath.Dir(p.dir.Workspace("x"))} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			name, ok := strings.CutSuffix(entry.Name(), ".json.lock")
			if !ok || state.ValidateName(name) != nil || recorded(p.dir.Orca(name)) || recorded(p.dir.Workspace(name)) || recorded(p.dir.Pool(name)) {
				continue
			}
			unlock, held, err := state.TryLock(filepath.Join(dir, entry.Name()))
			if err != nil {
				return removed, err
			}
			if !held {
				continue
			}
			err = state.Remove(filepath.Join(dir, entry.Name()))
			unlock()
			if err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

func removeIdleLock(path string) error {
	if !recorded(path) {
		return nil
	}
	unlock, held, err := state.TryLock(path)
	if err != nil || !held {
		return err
	}
	defer unlock()
	return state.Remove(path)
}

func recorded(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func recordNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if ok && !entry.IsDir() && state.ValidateName(name) == nil {
			names = append(names, name)
		}
	}
	return names, nil
}
