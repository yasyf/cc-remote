package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const (
	jobsSchema   = 1
	tokenRefresh = 45 * time.Minute
	claimPoll    = 5 * time.Second
)

type jobState string

const (
	jobProvisioning jobState = "provisioning"
	jobReady        jobState = "ready"
	jobHeld         jobState = "held"
	jobFailed       jobState = "failed"
)

type jobMember struct {
	SchemaVersion int       `json:"schemaVersion"`
	Name          string    `json:"name"`
	State         jobState  `json:"state"`
	Holder        string    `json:"holder,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	HeldAt        time.Time `json:"heldAt,omitzero"`
	TokenAt       time.Time `json:"tokenAt,omitzero"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type jobsStatus struct {
	Ready   int          `json:"ready"`
	Members []*jobMember `json:"members"`
}

type jobsKeep struct {
	Ready     []string `json:"ready"`
	Created   []string `json:"created,omitempty"`
	Failed    []string `json:"failed,omitempty"`
	Refreshed []string `json:"refreshed,omitempty"`
	Coalesced bool     `json:"coalesced,omitempty"`
}

type CommandExit struct{ Code int }

func (e *CommandExit) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

func (e *CommandExit) ExitCode() int { return e.Code }

type jobPool struct {
	cfg      *config.Config
	dir      state.Dir
	provider providers.Provider
	token    func(context.Context) (string, error)
	now      func() time.Time
	log      *slog.Logger
}

var jobName = func(prefix string) string { return prefix + "-" + strings.ToLower(rand.Text()[:8]) }

func openJobs(flags *selection) (*jobPool, error) {
	cfg, err := flags.load()
	if err != nil {
		return nil, err
	}
	provider, err := openProvider(cfg, flags.provider)
	if err != nil {
		return nil, err
	}
	return &jobPool{cfg: cfg, dir: cfg.State(), provider: provider, token: workspace.GitToken(cfg), now: time.Now, log: slog.Default()}, nil
}

func (p *jobPool) members() ([]*jobMember, error) {
	entries, err := os.ReadDir(filepath.Dir(p.dir.Job("x")))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var members []*jobMember
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		member, err := p.load(name)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	slices.SortFunc(members, func(a, b *jobMember) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.Name, b.Name))
	})
	return members, nil
}

func (p *jobPool) load(name string) (*jobMember, error) {
	member := &jobMember{}
	found, err := state.Load(p.dir.Job(name), member)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, fmt.Errorf("%s is not a job machine; adopt it, or let keep provision one", name)
	case member.SchemaVersion != jobsSchema || member.Name != name:
		return nil, fmt.Errorf("%s holds job schema %d for %q, not schema %d for %s", p.dir.Job(name), member.SchemaVersion, member.Name, jobsSchema, name)
	}
	return member, nil
}

func (p *jobPool) save(member *jobMember) error {
	member.UpdatedAt = p.now().UTC()
	return state.Save(p.dir.Job(member.Name), member)
}

func (p *jobPool) locked(run func() error) error {
	unlock, err := state.Lock(p.dir.JobsState())
	if err != nil {
		return err
	}
	defer unlock()
	return run()
}

func (p *jobPool) change(name string, from jobState, apply func(*jobMember)) error {
	return p.locked(func() error {
		member, err := p.load(name)
		if err != nil {
			return err
		}
		if member.State != from {
			return fmt.Errorf("%s is %s, not %s", name, member.State, from)
		}
		apply(member)
		return p.save(member)
	})
}

func (p *jobPool) adopt(names []string) error {
	return p.locked(func() error {
		for _, name := range names {
			if err := state.ValidateName(name); err != nil {
				return err
			}
			member := &jobMember{}
			found, err := state.Load(p.dir.Job(name), member)
			if err != nil {
				return err
			}
			if found && member.State != jobFailed {
				return fmt.Errorf("%s is already a %s job machine", name, member.State)
			}
			if err := p.save(&jobMember{SchemaVersion: jobsSchema, Name: name, State: jobReady, CreatedAt: p.now().UTC()}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *jobPool) claim(ctx context.Context, holder string, wait time.Duration) (string, error) {
	deadline := p.now().Add(wait)
	for {
		member, err := p.take(holder)
		if err != nil {
			return "", err
		}
		if member == nil && !p.now().Before(deadline) {
			return "", fmt.Errorf("no idle job machine for %s within %s", holder, wait)
		}
		if member == nil {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(claimPoll):
			}
			continue
		}
		if err := p.grant(ctx, member.Name); err != nil {
			p.log.Warn("a claimed job machine took no git credential and failed", "machine", member.Name, "holder", holder, "error", err)
			if err := p.change(member.Name, jobHeld, failed("credential: "+err.Error())); err != nil {
				return "", err
			}
			continue
		}
		p.log.Info("claimed a job machine", "machine", member.Name, "holder", holder)
		return member.Name, nil
	}
}

func (p *jobPool) take(holder string) (*jobMember, error) {
	var taken *jobMember
	err := p.locked(func() error {
		members, err := p.members()
		if err != nil {
			return err
		}
		index := slices.IndexFunc(members, func(member *jobMember) bool { return member.State == jobReady })
		if index < 0 {
			return nil
		}
		taken = members[index]
		taken.State, taken.Holder, taken.HeldAt = jobHeld, holder, p.now().UTC()
		return p.save(taken)
	})
	return taken, err
}

func (p *jobPool) grant(ctx context.Context, name string) error {
	repository, err := url.Parse(p.cfg.Repository)
	if err != nil {
		return err
	}
	if repository.Scheme != "https" {
		return nil
	}
	token, err := p.token(ctx)
	if err != nil {
		return err
	}
	credential := fmt.Sprintf("protocol=https\nhost=%s\nusername=x-access-token\npassword=%s\n\n", repository.Host, token)
	result, err := p.provider.Exec(ctx, name, []string{"git", "credential-cache", fmt.Sprintf("--timeout=%d", int((tokenRefresh + 20*time.Minute).Seconds())), "store"}, strings.NewReader(credential))
	if err == nil && result.ExitCode != 0 {
		err = fmt.Errorf("git credential-cache store exited %d", result.ExitCode)
	}
	if err != nil {
		return err
	}
	return p.change(name, jobHeld, func(member *jobMember) { member.TokenAt = p.now().UTC() })
}

func (p *jobPool) release(ctx context.Context, name string) error {
	member, err := p.load(name)
	if err != nil {
		return err
	}
	if member.State != jobHeld {
		return fmt.Errorf("%s is %s, not held", name, member.State)
	}
	if reset := p.cfg.Jobs.Reset; len(reset) > 0 {
		result, err := p.provider.Exec(ctx, name, reset, nil)
		if err == nil && result.ExitCode != 0 {
			err = fmt.Errorf("reset exited %d", result.ExitCode)
		}
		if err != nil {
			return errors.Join(err, p.change(name, jobHeld, failed("reset: "+err.Error())))
		}
	}
	p.log.Info("released a job machine", "machine", name, "holder", member.Holder)
	return p.change(name, jobHeld, func(member *jobMember) {
		member.State, member.Holder, member.HeldAt, member.TokenAt = jobReady, "", time.Time{}, time.Time{}
	})
}

func failed(reason string) func(*jobMember) {
	return func(member *jobMember) { member.State, member.Reason = jobFailed, reason }
}

func (p *jobPool) keep(ctx context.Context) (*jobsKeep, error) {
	unlock, held, err := state.TryLock(p.dir.JobsKeep())
	if err != nil {
		return nil, err
	}
	if !held {
		return &jobsKeep{Coalesced: true}, nil
	}
	defer unlock()
	kept := &jobsKeep{}
	var names []string
	err = p.locked(func() error {
		members, err := p.members()
		if err != nil {
			return err
		}
		for _, member := range members {
			switch {
			case member.State == jobProvisioning:
				failed("its provisioning keep ended before it finished")(member)
				if err := p.save(member); err != nil {
					return err
				}
			case member.State == jobReady:
				kept.Ready = append(kept.Ready, member.Name)
			case member.State == jobHeld && p.now().Sub(member.TokenAt) >= tokenRefresh:
				kept.Refreshed = append(kept.Refreshed, member.Name)
			}
		}
		for range p.cfg.Jobs.Ready - len(kept.Ready) {
			member := &jobMember{SchemaVersion: jobsSchema, Name: jobName(p.cfg.Jobs.Prefix), State: jobProvisioning, CreatedAt: p.now().UTC()}
			if err := p.save(member); err != nil {
				return err
			}
			names = append(names, member.Name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, name := range kept.Refreshed {
		errs = append(errs, p.grant(ctx, name))
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Go(func() {
			err := p.provision(ctx, name)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				kept.Failed = append(kept.Failed, name)
				errs = append(errs, err)
				return
			}
			kept.Created = append(kept.Created, name)
			kept.Ready = append(kept.Ready, name)
		})
	}
	wg.Wait()
	return kept, errors.Join(errs...)
}

func (p *jobPool) provision(ctx context.Context, name string) error {
	logPath := p.dir.JobLog(name)
	output, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return errors.Join(err, p.change(name, jobProvisioning, failed(err.Error())))
	}
	defer func() { _ = output.Close() }()
	argv := append(slices.Clone(p.cfg.Jobs.Provision), name)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir, command.Stdout, command.Stderr = filepath.Dir(p.cfg.Path), output, output
	if err := command.Run(); err != nil {
		reason := fmt.Sprintf("provision failed (%v); its output is in %s", err, logPath)
		destroyed := p.provider.Destroy(ctx, name)
		if errors.Is(destroyed, providers.ErrNotFound) {
			destroyed = nil
		}
		return errors.Join(fmt.Errorf("%s: %s", name, reason), destroyed, p.change(name, jobProvisioning, failed(reason)))
	}
	p.log.Info("provisioned a job machine", "machine", name)
	return p.change(name, jobProvisioning, func(member *jobMember) { member.State = jobReady })
}

func (p *jobPool) run(ctx context.Context, holder string, wait time.Duration, argv []string) error {
	name, err := p.claim(ctx, holder, wait)
	if err != nil {
		return err
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Env = append(os.Environ(), "SPRITE="+name)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	ran := command.Run()
	if exited, ok := errors.AsType[*exec.ExitError](ran); ok {
		ran = &CommandExit{exited.ExitCode()}
	}
	return errors.Join(ran, p.release(context.WithoutCancel(ctx), name))
}

func newJobsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Keep a pool of reusable job machines idle-ready, and claim and release them",
		Long: `jobs keeps reusable machines for heavy one-off work, such as builds, rebases and test packs, so that work
never runs on this machine. Unlike the Orca warm pool, a released job machine returns to the pool.

A claim takes the oldest ready machine, marks it held by the named holder, and stores a fresh git token from
git.token_command in the machine's git credential cache over standard input, never in a file or an argument.
keep refreshes the token of every machine held for 45 minutes or more. A release runs jobs.reset on the machine
and returns it to ready; a machine whose credential or reset fails is marked failed and leaves the pool.

keep provisions new machines named <jobs.prefix>-<id> until jobs.ready of them are ready, all at once, by running
jobs.provision with the new name as its last argument from the config file's directory. One keep runs at a time;
a keep that finds another running exits at once.`,
	}
	cmd.AddCommand(newJobsAdoptCmd(), newJobsClaimCmd(), newJobsReleaseCmd(), newJobsRunCmd(), newJobsKeepCmd(), newJobsStatusCmd())
	return cmd
}

func newJobsAdoptCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "adopt <machine>...",
		Short: "Record machines provisioned elsewhere, or failed machines you repaired, as ready job machines",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			pool, err := openJobs(&flags)
			if err != nil {
				return err
			}
			return pool.adopt(args)
		},
	}
	flags.bind(cmd)
	return cmd
}

func newJobsClaimCmd() *cobra.Command {
	var flags selection
	var holder string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "claim",
		Short: "Hold the oldest ready job machine and print its name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := openJobs(&flags)
			if err != nil {
				return err
			}
			name, err := pool.claim(cmd.Context(), holder, wait)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), name)
			return err
		},
	}
	flags.bind(cmd)
	bindHolder(cmd, &holder, &wait)
	return cmd
}

func newJobsReleaseCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "release <machine>",
		Short: "Reset a held job machine and return it to ready",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pool, err := openJobs(&flags)
			if err != nil {
				return err
			}
			return pool.release(cmd.Context(), args[0])
		},
	}
	flags.bind(cmd)
	return cmd
}

func newJobsRunCmd() *cobra.Command {
	var flags selection
	var holder string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "run -- <command> [args...]",
		Short: "Claim a job machine, run a command here with SPRITE set to its name, then release it",
		Long:  "run exits with the command's own status, and releases the machine however the command ends.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pool, err := openJobs(&flags)
			if err != nil {
				return err
			}
			return pool.run(cmd.Context(), holder, wait, args)
		},
	}
	flags.bind(cmd)
	bindHolder(cmd, &holder, &wait)
	return cmd
}

func newJobsKeepCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "keep",
		Short: "Provision job machines until jobs.ready are ready, and refresh held machines' git tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := openJobs(&flags)
			if err != nil {
				return err
			}
			kept, err := pool.keep(cmd.Context())
			if kept == nil {
				return err
			}
			return errors.Join(err, emit(cmd.OutOrStdout(), kept))
		},
	}
	flags.bind(cmd)
	return cmd
}

func newJobsStatusCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print every job machine's record as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := openJobs(&flags)
			if err != nil {
				return err
			}
			members, err := pool.members()
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), jobsStatus{Ready: pool.cfg.Jobs.Ready, Members: members})
		},
	}
	flags.bind(cmd)
	return cmd
}

func bindHolder(cmd *cobra.Command, holder *string, wait *time.Duration) {
	cmd.Flags().StringVar(holder, "holder", "", "the lane or job holding the machine, shown in status")
	cmd.Flags().DurationVar(wait, "wait", 10*time.Minute, "how long to wait for a ready machine")
	_ = cmd.MarkFlagRequired("holder")
}
