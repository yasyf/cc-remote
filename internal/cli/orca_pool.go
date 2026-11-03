package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/sprites"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const (
	orcaPoolSchema       = 1
	orcaAllocationSchema = 1
	allocationWarm       = "warm"
	allocationCreated    = "created"
	fillTimeout          = 20 * time.Minute
)

type memberState string

const (
	memberProvisioning memberState = "provisioning"
	memberReady        memberState = "ready"
	memberClaimed      memberState = "claimed"
	memberPrepared     memberState = "prepared"
	memberFailed       memberState = "failed"
	memberAbandoned    memberState = "abandoned"
	memberRefused      memberState = "refused"
)

type orcaMember struct {
	SchemaVersion int            `json:"schemaVersion"`
	Name          string         `json:"name"`
	Provider      string         `json:"provider"`
	Profile       string         `json:"profile"`
	Key           string         `json:"key"`
	State         memberState    `json:"state"`
	Lane          string         `json:"lane,omitempty"`
	Ref           string         `json:"ref,omitempty"`
	Head          string         `json:"head,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	Failure       *orcaFailure   `json:"failure,omitempty"`
	Pregrant      *orca.Pregrant `json:"pregrant,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

type failureClass string

const (
	classExit     failureClass = "exit"
	classHTTP     failureClass = "http"
	classDeadline failureClass = "deadline"
	classCanceled failureClass = "canceled"
	classOther    failureClass = "other"
)

type orcaFailure struct {
	Step       string       `json:"step,omitempty"`
	Class      failureClass `json:"class"`
	Exit       *int         `json:"exit"`
	HTTPStatus *int         `json:"httpStatus"`
	Bytes      int          `json:"bytes"`
	SHA256     string       `json:"sha256"`
}

type reservation struct {
	member *orcaMember
	unlock func()
}

type spareFailure struct {
	member *orcaMember
}

type orcaPool struct {
	session *workspace.Session
	dir     state.Dir
	key     string
	target  int
}

type orcaAllocation struct {
	SchemaVersion int                `json:"schemaVersion"`
	Lane          string             `json:"lane"`
	Workspace     string             `json:"workspace"`
	Allocation    string             `json:"allocation"`
	Ref           string             `json:"ref"`
	Head          string             `json:"head,omitempty"`
	Replenish     *orcaReplenish     `json:"replenish"`
	Task          *orcaTask          `json:"task"`
	HookDrift     []images.HookDrift `json:"captainHookDrift,omitempty"`
}

type orcaReplenish struct {
	Target  int          `json:"target"`
	PID     int          `json:"pid,omitempty"`
	Failure *orcaFailure `json:"failure,omitempty"`
}

type orcaFill struct {
	Key       string   `json:"key"`
	Target    int      `json:"target"`
	Ready     []string `json:"ready"`
	Created   []string `json:"created,omitempty"`
	Abandoned []string `json:"abandoned,omitempty"`
	Refused   []string `json:"refused,omitempty"`
}

type orcaPoolStatus struct {
	Key     string        `json:"key"`
	Target  int           `json:"target"`
	Members []*orcaMember `json:"members"`
}

var (
	spareName   = func() string { return "pool-" + strings.ToLower(rand.Text()[:16]) }
	fillCommand = func(config, provider, profile string) (*exec.Cmd, error) {
		helper, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return exec.Command(helper, "orca", "pool", "fill", "--config", config, "--provider", provider, "--profile", profile), nil
	}
)

func warmable(session *workspace.Session) error {
	if session.Kind != sprites.Name {
		return fmt.Errorf("the warm pool keeps only %s workspaces, not %s ones", sprites.Name, session.Kind)
	}
	return session.Poolable()
}

func openPool(session *workspace.Session) *orcaPool {
	return &orcaPool{session: session, dir: session.Config.State(), key: session.PoolKey(), target: *session.Config.Orca.Pool.Ready}
}

func (l *orcaLaunch) allocate(ctx context.Context, out io.Writer, session *workspace.Session, runner orca.Runner, runtime orca.Runtime, lane string, finish func(context.Context, orcaDriver, *orcaTask) error) error {
	dir := session.Config.State()
	unlock, err := claimTask(dir, lane)
	if err != nil {
		return err
	}
	defer unlock()
	if err := absentTask(dir, lane); err != nil {
		return err
	}
	source := l.source(session.Config)
	if err := source.Validate(); err != nil {
		return err
	}
	pool := openPool(session)
	if err := pool.admit(lane); err != nil {
		return err
	}
	if _, err := selectHelper(session); err != nil {
		return err
	}
	allocation := &orcaAllocation{SchemaVersion: orcaAllocationSchema, Lane: lane, Ref: source.Ref}
	var failed []error
	for {
		member, release, err := pool.claim(ctx, lane, source.Ref)
		if err != nil {
			return withEarlier(failed, err)
		}
		allocation.Replenish = pool.replenish()
		if member == nil {
			break
		}
		allocation.Workspace, allocation.Allocation = member.Name, allocationWarm
		allocation.Task, err = l.prime(ctx, session, runner, runtime, member.Name, openSpare, member.Pregrant, finish)
		err = errors.Join(err, pool.settle(member, allocation.Task, err))
		release()
		if err == nil || allocation.Task != nil && allocation.Task.Terminal != "" {
			allocation.Head, allocation.HookDrift = allocation.Task.BaseCommit, allocation.Task.drift
			return errors.Join(withEarlier(failed, err), emit(out, allocation))
		}
		session.Log.Warn("a claimed warm workspace failed before its worker started, so the lane tries the next", "lane", lane, "workspace", member.Name, "failure", failureOf(err).describe())
		failed = append(failed, fmt.Errorf("lane %s claimed warm workspace %s, which stays claimed as failed: %w", lane, member.Name, err))
	}
	allocation.Workspace, allocation.Allocation = lane, allocationCreated
	task, err := l.prime(ctx, session, runner, runtime, lane, openCreated, nil, finish)
	if task == nil {
		return withEarlier(failed, err)
	}
	allocation.Task, allocation.Head, allocation.HookDrift = task, task.BaseCommit, task.drift
	return errors.Join(withEarlier(failed, err), emit(out, allocation))
}

func withEarlier(earlier []error, err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(append(earlier, err)...)
}

func (p *orcaPool) records() ([]*orcaMember, error) {
	entries, err := os.ReadDir(filepath.Dir(p.dir.Pool("x")))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var members []*orcaMember
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		member, err := p.load(name)
		if err != nil {
			return nil, err
		}
		if member != nil {
			members = append(members, member)
		}
	}
	slices.SortFunc(members, func(a, b *orcaMember) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.Name, b.Name))
	})
	return members, nil
}

func (p *orcaPool) members() ([]*orcaMember, error) {
	records, err := p.records()
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(records, func(member *orcaMember) bool {
		return member.Provider != p.session.Kind || member.Profile != p.session.Profile
	}), nil
}

func (p *orcaPool) load(name string) (*orcaMember, error) {
	member := &orcaMember{}
	found, err := state.Load(p.dir.Pool(name), member)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, nil
	case member.SchemaVersion != orcaPoolSchema || member.Name != name:
		return nil, fmt.Errorf("%s holds pool schema %d for %q, not schema %d for %s", p.dir.Pool(name), member.SchemaVersion, member.Name, orcaPoolSchema, name)
	}
	return member, nil
}

func (p *orcaPool) save(member *orcaMember) error {
	member.UpdatedAt = p.session.Now().UTC()
	return state.Save(p.dir.Pool(member.Name), member)
}

func (p *orcaPool) admit(lane string) error {
	records, err := p.records()
	if err != nil {
		return err
	}
	for _, member := range records {
		switch {
		case member.Name == lane:
			return fmt.Errorf("%s names a warm pool workspace, not a lane; request the worker under a lane name of its own", lane)
		case member.Lane == lane:
			return fmt.Errorf("lane %s already claimed warm workspace %s, now %s; a lane gets one first worker", lane, member.Name, member.State)
		}
	}
	return nil
}

func (p *orcaPool) claim(ctx context.Context, lane, ref string) (*orcaMember, func(), error) {
	members, err := p.members()
	if err != nil {
		return nil, nil, err
	}
	for _, listed := range members {
		if listed.Key != p.key || listed.State != memberReady {
			continue
		}
		member, release, err := p.take(ctx, listed.Name, lane, ref)
		if err != nil || member != nil {
			return member, release, err
		}
	}
	return nil, nil, nil
}

func (p *orcaPool) take(ctx context.Context, name, lane, ref string) (*orcaMember, func(), error) {
	unlock, held, err := state.TryLock(p.dir.Orca(name) + ".lock")
	if err != nil || !held {
		return nil, nil, err
	}
	member, err := p.unused(ctx, name, lane, ref)
	if err != nil || member == nil {
		unlock()
		return nil, nil, err
	}
	p.session.Log.Info("claimed a warm workspace", "lane", lane, "workspace", name, "ref", ref)
	return member, unlock, nil
}

func (p *orcaPool) unused(ctx context.Context, name, lane, ref string) (*orcaMember, error) {
	listed, err := p.load(name)
	if err != nil || listed == nil || listed.Key != p.key || listed.State != memberReady {
		return nil, err
	}
	reason, err := p.refusal(ctx, listed)
	if err != nil || reason != "" {
		return nil, errors.Join(err, p.refuse(name, reason))
	}
	var claimed *orcaMember
	err = p.locked(func() error {
		member, err := p.load(name)
		if err != nil || member == nil || member.Key != p.key || member.State != memberReady {
			return err
		}
		member.State, member.Lane, member.Ref = memberClaimed, lane, ref
		if err := p.save(member); err != nil {
			return err
		}
		claimed = member
		return nil
	})
	return claimed, err
}

func (p *orcaPool) refuse(name, reason string) error {
	if reason == "" {
		return nil
	}
	return p.locked(func() error {
		member, err := p.load(name)
		if err != nil || member == nil || member.Key != p.key || member.State != memberReady {
			return err
		}
		p.session.Log.Info("refused a warm workspace without touching it", "workspace", name, "reason", reason)
		member.State, member.Reason = memberRefused, reason
		return p.save(member)
	})
}

func (p *orcaPool) locked(run func() error) error {
	unlock, err := state.Lock(p.dir.PoolState(p.key))
	if err != nil {
		return err
	}
	defer unlock()
	return run()
}

func (p *orcaPool) refusal(ctx context.Context, member *orcaMember) (string, error) {
	if member.Pregrant != nil && !p.session.Config.Trusted() {
		return "it holds a Codex trust pregrant that orca.trust no longer authorizes", nil
	}
	switch _, err := os.Lstat(p.dir.Orca(member.Name)); {
	case err == nil:
		return "an Orca task is already recorded for it, so it is not unused", nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	record, err := p.session.Recorded(member.Name)
	if err != nil {
		return "its workspace record is missing, unreadable, or another provider's or profile's", nil
	}
	if record.Name != member.Name {
		return fmt.Sprintf("its workspace record names %s, not %s", record.Name, member.Name), nil
	}
	if record.Unverified || record.Machine == "" {
		return "its create never confirmed a machine", nil
	}
	machine, err := p.session.Provider.Get(ctx, record.Machine)
	switch {
	case errors.Is(err, providers.ErrNotFound):
		return fmt.Sprintf("%s machine %s is gone", record.Provider, record.Machine), nil
	case err != nil:
		return "", fmt.Errorf("observe warm workspace %s before claiming it: %w", member.Name, err)
	case machine.ID != record.Machine || machine.Provider != record.Provider || machine.Labels[workspace.LabelWorkspace] != member.Name || machine.Labels[workspace.LabelPool] != p.key:
		return fmt.Sprintf("%s machine %s does not carry the ownership labels of warm workspace %s", record.Provider, record.Machine, member.Name), nil
	}
	return "", nil
}

func (p *orcaPool) settle(member *orcaMember, task *orcaTask, err error) error {
	member.State, member.Failure = memberPrepared, nil
	if err != nil {
		member.State, member.Failure = memberFailed, failureOf(err)
	}
	if task != nil {
		member.Head = task.BaseCommit
	}
	return p.save(member)
}

func (p *orcaPool) fill(ctx context.Context) (*orcaFill, error) {
	fill := &orcaFill{Key: p.key, Target: p.target}
	helper, err := selectHelper(p.session)
	if err != nil {
		return fill, err
	}
	if helper != nil {
		p.session.Alongside = func(ctx context.Context, machine string) error {
			return ensureHelper(ctx, p.session, machine, helper)
		}
	}
	reserved, err := p.shortfall(ctx, fill)
	if err != nil {
		return fill, err
	}
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, spare := range reserved {
		wg.Go(func() {
			err := p.build(ctx, spare)
			if err != nil {
				p.session.Log.Warn("a warm workspace create failed, and the next claim's fill replaces it", "error", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				fill.Created = append(fill.Created, spare.member.Name)
				fill.Ready = append(fill.Ready, spare.member.Name)
			}
			errs = append(errs, err)
		})
	}
	wg.Wait()
	slices.Sort(fill.Created)
	return fill, errors.Join(errs...)
}

func (p *orcaPool) build(ctx context.Context, spare *reservation) error {
	defer spare.unlock()
	member := spare.member
	result, err := p.session.CreateSpare(ctx, member.Name, workspace.Source{Ref: p.session.Config.Ref})
	if err == nil {
		member.Pregrant, err = p.pregrant(ctx, result)
	}
	if err != nil {
		member.State, member.Failure = memberFailed, failureOf(err)
		var refusal orca.GrantRefusal
		if errors.As(err, &refusal) {
			member.Reason = refusal.Error()
		}
		return errors.Join(&spareFailure{member: member}, p.save(member))
	}
	member.State = memberReady
	return p.save(member)
}

func (p *orcaPool) reserve() (*reservation, error) {
	member := &orcaMember{SchemaVersion: orcaPoolSchema, Name: spareName(), Provider: p.session.Kind, Profile: p.session.Profile, Key: p.key, State: memberProvisioning, CreatedAt: p.session.Now().UTC()}
	unlock, err := state.Lock(p.dir.Orca(member.Name) + ".lock")
	if err != nil {
		return nil, err
	}
	if err := p.save(member); err != nil {
		unlock()
		return nil, err
	}
	return &reservation{member: member, unlock: unlock}, nil
}

func (p *orcaPool) pregrant(ctx context.Context, result *workspace.Result) (*orca.Pregrant, error) {
	if !p.session.Config.Trusted() {
		return nil, nil
	}
	grant, selected, err := codexGrant(p.session, result.Machine, result.ProjectRoot)
	if err != nil || !selected {
		return nil, err
	}
	if err := checkClean(ctx, p.session, "check the warm checkout before its grant", result.Name, result.Machine); err != nil {
		return nil, err
	}
	return grant.Fill(ctx)
}

func (p *orcaPool) shortfall(ctx context.Context, fill *orcaFill) ([]*reservation, error) {
	eligible, err := p.census(ctx, fill)
	if err != nil {
		return nil, err
	}
	var reserved []*reservation
	err = p.locked(func() error {
		members, err := p.members()
		if err != nil {
			return err
		}
		creating := 0
		for _, member := range members {
			switch {
			case member.Key != p.key:
			case member.State == memberReady:
				if ok, seen := eligible[member.Name]; ok || !seen {
					fill.Ready = append(fill.Ready, member.Name)
				}
			case member.State == memberProvisioning:
				unlock, held, err := state.TryLock(p.dir.Orca(member.Name) + ".lock")
				if err != nil {
					return err
				}
				if !held {
					creating++
					continue
				}
				unlock()
				member.State, member.Reason = memberAbandoned, "the fill that created it ended before it recorded the create's outcome, so its machine is left as it is"
				if err := p.save(member); err != nil {
					return err
				}
				fill.Abandoned = append(fill.Abandoned, member.Name)
			}
		}
		for range p.target - len(fill.Ready) - creating {
			spare, err := p.reserve()
			if err != nil {
				return err
			}
			reserved = append(reserved, spare)
		}
		return nil
	})
	if err != nil {
		for _, spare := range reserved {
			spare.unlock()
		}
		return nil, err
	}
	return reserved, nil
}

func (p *orcaPool) census(ctx context.Context, fill *orcaFill) (map[string]bool, error) {
	members, err := p.members()
	if err != nil {
		return nil, err
	}
	eligible := map[string]bool{}
	for _, member := range members {
		if member.Key != p.key || member.State != memberReady {
			continue
		}
		unlock, held, err := state.TryLock(p.dir.Orca(member.Name) + ".lock")
		if err != nil {
			return nil, err
		}
		if !held {
			eligible[member.Name] = false
			continue
		}
		unlock()
		reason, err := p.refusal(ctx, member)
		switch {
		case err != nil:
			return nil, err
		case reason == "":
			eligible[member.Name] = true
		default:
			if err := p.refuse(member.Name, reason); err != nil {
				return nil, err
			}
			fill.Refused = append(fill.Refused, member.Name)
		}
	}
	return eligible, nil
}

func (p *orcaPool) replenish() *orcaReplenish {
	started := &orcaReplenish{Target: p.target}
	if p.target == 0 {
		return started
	}
	pid, err := p.spawn()
	if err != nil {
		started.Failure = failureOf(err)
		p.session.Log.Error("the warm pool fill did not start", "bytes", started.Failure.Bytes, "sha256", started.Failure.SHA256)
		return started
	}
	started.PID = pid
	return started
}

func (p *orcaPool) spawn() (int, error) {
	cmd, err := fillCommand(p.session.Config.Path, p.session.Kind, p.session.Profile)
	if err != nil {
		return 0, err
	}
	logPath := p.dir.PoolLog(p.key)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, err
	}
	output, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = output.Close() }()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = fillEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start the warm pool fill: %w", err)
	}
	pid := cmd.Process.Pid
	return pid, cmd.Process.Release()
}

func (e *spareFailure) Error() string {
	return "create warm workspace " + e.member.Name + " failed with " + e.member.Failure.describe() + " and is kept only as that metadata in orca pool status"
}

func (f *orcaFailure) describe() string {
	exit, status := "unknown", "unknown"
	if f.Exit != nil {
		exit = strconv.Itoa(*f.Exit)
	}
	if f.HTTPStatus != nil {
		status = strconv.Itoa(*f.HTTPStatus)
	}
	step := ""
	if f.Step != "" {
		step = " at " + f.Step
	}
	return fmt.Sprintf("exit %s and HTTP status %s%s; its %d-byte error, class %s, has SHA-256 %s", exit, status, step, f.Bytes, f.Class, f.SHA256)
}

func withheld(err error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		parts := make([]error, 0, len(joined.Unwrap()))
		for _, part := range joined.Unwrap() {
			parts = append(parts, withheld(part))
		}
		return errors.Join(parts...)
	}
	var failed *spareFailure
	if errors.As(err, &failed) {
		return failed
	}
	return errors.New("the warm pool fill failed with " + failureOf(err).describe() + ", and its text is withheld")
}

func fillEnv(environ []string) []string {
	credentials := orca.CredentialEnv()
	return slices.DeleteFunc(append([]string{}, environ...), func(pair string) bool {
		name, _, _ := strings.Cut(pair, "=")
		return slices.Contains(credentials, name)
	})
}

func failureOf(err error) *orcaFailure {
	text := err.Error()
	sum := sha256.Sum256([]byte(text))
	failure := &orcaFailure{Bytes: len(text), SHA256: hex.EncodeToString(sum[:])}
	var phase *workspace.PhaseError
	if errors.As(err, &phase) {
		failure.Step = phase.Phase
	}
	var create *sprites.CreateError
	if errors.As(err, &create) {
		failure.Step = string(create.Step)
	}
	var answered *sprites.StatusError
	if errors.As(err, &answered) {
		failure.HTTPStatus = &answered.Status
	}
	var command *providers.CommandError
	var exited interface{ ExitCode() int }
	switch {
	case errors.As(err, &command):
		failure.Exit = &command.Result.ExitCode
	case errors.As(err, &exited):
		code := exited.ExitCode()
		failure.Exit = &code
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		failure.Class = classDeadline
	case errors.Is(err, context.Canceled):
		failure.Class = classCanceled
	case failure.Exit != nil:
		failure.Class = classExit
	case failure.HTTPStatus != nil:
		failure.Class = classHTTP
	default:
		failure.Class = classOther
	}
	return failure
}

func newOrcaPoolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Keep unused agent-free Sprite workspaces ready for orca prepare --warm",
	}
	cmd.AddCommand(newOrcaPoolFillCmd(), newOrcaPoolStatusCmd())
	return cmd
}

func newOrcaPoolFillCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "fill",
		Short: "Create agent-free warm workspaces until orca.pool.ready of them are unused, then exit",
		Long: `fill creates workspaces for the selected Sprite provider and profile until orca.pool.ready of them are
recorded unused and still eligible, then exits. Each is a plain create labelled with the pool's compatibility
key: tools, plugins, and a shallow checkout of config.ref, with no Orca runtime, task, worker, or API key. Fills
run side by side: each counts the ready workspaces and the ones other fills are still creating, reserves the
shortfall, and creates those workspaces at once, so a claim that leaves the pool one short makes one create. A
failed create is recorded as failed, logged, and left for the next claim's fill to replace, and its error is
shown only as the step that failed, its class, exit, HTTP status, length, and SHA-256. The fill stops at a 20 minute deadline.
prepare --warm starts this command detached once it has claimed, appending its output to the pool's fill log.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			if err := warmable(session); err != nil {
				return err
			}
			session.Stderr = io.Discard
			ctx, cancel := context.WithTimeout(cmd.Context(), fillTimeout)
			defer cancel()
			fill, err := openPool(session).fill(ctx)
			if err != nil {
				err = withheld(err)
			}
			if fill == nil {
				return err
			}
			return errors.Join(err, emit(cmd.OutOrStdout(), fill))
		},
	}
	flags.bind(cmd)
	return cmd
}

func newOrcaPoolStatusCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print the warm pool's records for the selected provider and profile as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := flags.open()
			if err != nil {
				return err
			}
			if err := warmable(session); err != nil {
				return err
			}
			pool := openPool(session)
			members, err := pool.members()
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), orcaPoolStatus{Key: pool.key, Target: pool.target, Members: members})
		},
	}
	flags.bind(cmd)
	return cmd
}
