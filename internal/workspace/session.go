package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yasyf/cc-remote/internal/budget"
	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/identity"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/tailnet"
)

const (
	LabelWorkspace = "cc-remote/workspace"
	LabelSpare     = "cc-remote/spare"
	LabelProfile   = "cc-remote/profile"
	tailnetTimeout = 60 * time.Second
)

type Session struct {
	Config   *config.Config
	Provider providers.Provider
	Kind     string
	Profile  string
	Platform Platform
	Pool     Pool
	State    state.Dir
	Enroller *tailnet.Enroller
	Log      *slog.Logger
	Stderr   io.Writer
	Token    func(context.Context) (string, error)
	Now      func() time.Time
	Refill   func() error

	profile config.Profile
	labels  []LabelledEnv
}

func Open(cfg *config.Config, provider providers.Provider, kind, profile string, platform Platform) (*Session, error) {
	spec, err := cfg.ProfileNamed(profile)
	if err != nil {
		return nil, err
	}
	machine := spec.Machine[kind]
	rate, err := provider.Rate(providers.Spec{Name: "rate", Profile: profile, Image: machine.Image, Size: machine.Size, Region: machine.Region})
	if err != nil {
		return nil, err
	}
	pool, err := NewPool(cfg, kind, profile, budget.Rate(rate))
	if err != nil {
		return nil, err
	}
	s := &Session{
		Config:   cfg,
		Provider: provider,
		Kind:     kind,
		Profile:  profile,
		Platform: platform,
		Pool:     pool,
		State:    cfg.State(),
		Log:      slog.Default(),
		Stderr:   os.Stderr,
		Now:      time.Now,
		profile:  spec,
	}
	s.Token = s.gitToken
	for _, forward := range cfg.Forwards {
		s.labels = append(s.labels, LabelledEnv{Label: forward.Label, Env: forward.Env})
	}
	if cfg.Tailnet != nil {
		s.Enroller = &tailnet.Enroller{
			Connect:  s.tailnetClient,
			Bindings: tailnet.Bindings{Dir: s.State.Tailnet()},
			Daemon:   platform.Daemon,
			Log:      s.Log,
		}
	}
	return s, nil
}

func (s *Session) tailnetClient(ctx context.Context) (*tailnet.Client, error) {
	credential, err := tailnet.LoadCredential(ctx, s.Config.Tailnet.KeychainService)
	if err != nil {
		return nil, err
	}
	return &tailnet.Client{
		Tag:        s.Config.Tailnet.Tag,
		Base:       s.Config.Tailnet.API,
		Credential: credential,
		HTTP:       &http.Client{Timeout: tailnetTimeout},
	}, nil
}

func (s *Session) gitToken(ctx context.Context) (string, error) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := os.Getenv(name); token != "" {
			return token, nil
		}
	}
	command := s.Config.Git.TokenCommand
	out, err := exec.CommandContext(ctx, command[0], command[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("no GH_TOKEN or GITHUB_TOKEN, and %q failed: %w", strings.Join(command, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *Session) ProjectRoot() string {
	return s.Config.ProjectRoot(s.Kind)
}

func (s *Session) spec(name string, labels map[string]string) providers.Spec {
	machine := s.profile.Machine[s.Kind]
	labels[LabelProfile] = s.Profile
	return providers.Spec{Name: name, Profile: s.Profile, Image: machine.Image, Size: machine.Size, Region: machine.Region, Labels: labels}
}

type runner struct {
	session *Session
	machine string
}

func (r runner) Run(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	return r.session.run(ctx, r.machine, script, stdin)
}

func (s *Session) run(ctx context.Context, machine, script string, stdin io.Reader) ([]byte, error) {
	result, err := s.Provider.Exec(ctx, machine, []string{"sh", "-c", script}, stdin)
	if err != nil {
		return nil, err
	}
	if _, err := s.Stderr.Write(result.Stderr); err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return result.Stdout, fmt.Errorf("a script on %s exited %d", machine, result.ExitCode)
	}
	return result.Stdout, nil
}

func (s *Session) verifyTailnet(ctx context.Context) error {
	if s.Enroller == nil {
		return nil
	}
	_, err := s.Enroller.Client(ctx)
	return err
}

var errNotRecorded = errors.New("no workspace is recorded")

func (s *Session) record(name string) (*Record, error) {
	record := &Record{}
	found, err := state.Load(s.State.Workspace(name), record)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w for %s under %s", errNotRecorded, name, s.State)
	}
	if record.Provider != s.Kind || record.Profile != s.Profile {
		return nil, fmt.Errorf("workspace %s is a %s/%s workspace, not %s/%s", name, record.Provider, record.Profile, s.Kind, s.Profile)
	}
	return record, nil
}

func (s *Session) save(record *Record) error {
	record.UpdatedAt = s.Now()
	return state.Save(s.State.Workspace(record.Name), record)
}

func (s *Session) forget(name string) error {
	return state.Remove(s.State.Workspace(name))
}

func (s *Session) Create(ctx context.Context, name string, source Source) (*Result, error) {
	if err := state.ValidateName(name); err != nil {
		return nil, err
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	held, err := s.State.Hold(name)
	if err != nil {
		return nil, err
	}
	defer held.Release()
	if err := s.verifyTailnet(ctx); err != nil {
		return nil, err
	}
	recorded, err := s.record(name)
	if err != nil && !errors.Is(err, errNotRecorded) {
		return nil, err
	}
	if err := s.reconcile(name, recorded); err != nil {
		return nil, err
	}
	if recorded == nil {
		if err := s.unbound(name); err != nil {
			return nil, err
		}
	}
	now := s.Now()
	machine, assignment, err := s.Pool.Assign(name, now)
	if err != nil {
		return nil, err
	}
	if assignment == Reclaim {
		s.Log.Info("reattaching the spare this workspace already claimed", "workspace", name, "machine", machine)
		return s.restore(ctx, held, recorded)
	}
	record := &Record{Name: name, Provider: s.Kind, Profile: s.Profile, Source: source, Machine: machine, Claimed: assignment == NewClaim, CreatedAt: now}
	if err := s.save(record); err != nil {
		return nil, err
	}
	if !record.Claimed {
		s.Log.Info("creating", "workspace", name, "provider", s.Kind, "profile", s.Profile)
		if _, err := s.Provider.Create(ctx, s.spec(machine, map[string]string{LabelWorkspace: name})); err != nil {
			return nil, errors.Join(err, s.Pool.Retire(machine, s.Now()), s.forget(name))
		}
		s.Log.Info("created", "machine", machine)
	}
	result, err := s.provision(ctx, held, record)
	if err == nil && record.Claimed {
		err = s.Pool.Activate(machine, s.Now())
	}
	if err == nil {
		err = s.save(record)
	}
	if err != nil {
		return nil, errors.Join(err, s.abandon(context.WithoutCancel(ctx), held, record))
	}
	return result, nil
}

func (s *Session) reconcile(name string, recorded *Record) error {
	ledger, err := s.Pool.Ledger.Read()
	if err != nil {
		return err
	}
	claimed, err := s.Pool.Claimed(name)
	if err != nil {
		return err
	}
	switch {
	case recorded == nil && claimed != "":
		return fmt.Errorf("%s holds a claim on %s from a create that never finished and left no record under %s, so destroy %s before creating again", name, claimed, s.State, name)
	case recorded == nil:
		return nil
	case recorded.Claimed && claimed == recorded.Machine:
		return nil
	case !recorded.Claimed && ledger.Vacant(name) != nil:
		return ledger.Vacant(name)
	}
	return fmt.Errorf("%s is already recorded under %s as machine %s, which ledger %s does not know; resolve the two before creating again", name, s.State, recorded.Machine, s.Pool.Ledger.Path)
}

func (s *Session) unbound(name string) error {
	if s.Enroller == nil {
		return nil
	}
	binding, err := s.Enroller.Bindings.Read(name)
	if err != nil {
		return err
	}
	if binding != (tailnet.Binding{}) {
		return fmt.Errorf("%s is still bound to tailnet node %v from an earlier attempt whose cleanup did not finish, so destroy %s before creating again", name, binding, name)
	}
	return nil
}

func (s *Session) provision(ctx context.Context, held *state.Held, record *Record) (*Result, error) {
	machine := record.Machine
	if record.Claimed {
		s.Log.Info("claimed a prepared spare", "workspace", record.Name, "machine", machine, "provider", s.Kind, "profile", s.Profile)
		s.replaceClaimed()
		if err := s.Provider.Wake(ctx, machine); err != nil {
			return nil, err
		}
		if _, err := s.run(ctx, machine, identity.FreshScript(s.Platform.HostKeys), nil); err != nil {
			return nil, err
		}
		s.Log.Info("renewed its identity", "machine", machine)
	}
	if err := s.install(ctx, record); err != nil {
		return nil, err
	}
	if record.Claimed {
		if err := s.warm(ctx, machine); err != nil {
			return nil, err
		}
	}
	if err := s.enroll(ctx, held, record); err != nil {
		return nil, err
	}
	return s.connect(ctx, record)
}

func (s *Session) install(ctx context.Context, record *Record) error {
	machine, root := record.Machine, s.ProjectRoot()
	if err := record.Source.Validate(); err != nil {
		return err
	}
	token, err := s.checkoutToken(ctx)
	if err != nil {
		return err
	}
	if _, err := s.run(ctx, machine, CheckoutScript(root, s.Config.Repository, record.Source, s.profile.Checkout == config.Shallow), strings.NewReader(token+"\n")); err != nil {
		return err
	}
	s.Log.Info("checked out", "machine", machine, "ref", record.Source.Ref, "head", record.Source.Head)
	env, err := s.forwards(record)
	if err != nil {
		return err
	}
	if _, err := s.run(ctx, machine, RefreshScript(root, env, s.profile.Prepare), nil); err != nil {
		return err
	}
	s.Log.Info("prepared", "machine", machine)
	if err := s.bootstrap(ctx, machine, env, token); err != nil {
		return err
	}
	s.Log.Info("bootstrapped", "machine", machine)
	return nil
}

func (s *Session) checkoutToken(ctx context.Context) (string, error) {
	if !TokenAllowed(s.Config.Repository) {
		return "", nil
	}
	return s.Token(ctx)
}

func (s *Session) forwards(record *Record) ([]string, error) {
	forwards, err := AllocateForwards(s.labels, record.Forwards)
	if err != nil {
		return nil, err
	}
	record.Forwards = forwards
	return ForwardEnv(forwards, s.labels), nil
}

func (s *Session) bootstrap(ctx context.Context, machine string, env []string, token string) error {
	if s.Config.Bootstrap == "" {
		return nil
	}
	script, err := os.ReadFile(s.Config.ScriptPath(s.Config.Bootstrap))
	if err != nil {
		return err
	}
	if _, err := s.run(ctx, machine, WriteBootstrapScript, bytes.NewReader(script)); err != nil {
		return err
	}
	_, err = s.run(ctx, machine, BootstrapScript(s.ProjectRoot(), env), strings.NewReader(token+"\n"))
	return err
}

func (s *Session) warm(ctx context.Context, machine string) error {
	if _, err := s.run(ctx, machine, WarmScript(s.ProjectRoot(), s.profile.WarmInputs, s.profile.Warm), nil); err != nil {
		return err
	}
	s.Log.Info("warmed", "machine", machine)
	return nil
}

func (s *Session) enroll(ctx context.Context, held *state.Held, record *Record) error {
	if s.Enroller == nil {
		return nil
	}
	node, err := s.Enroller.Enroll(ctx, held, runner{s, record.Machine})
	record.Tailnet = node
	return err
}

func (s *Session) connect(ctx context.Context, record *Record) (*Result, error) {
	target, err := s.Provider.SSHTarget(ctx, record.Machine)
	if err != nil {
		return nil, err
	}
	return &Result{
		SchemaVersion: SchemaVersion,
		Name:          record.Name,
		Provider:      s.Kind,
		Profile:       s.Profile,
		Source:        record.Source,
		Machine:       record.Machine,
		ProjectRoot:   s.ProjectRoot(),
		SSH:           sshFromTarget(target),
		Forwards:      record.Forwards,
		Tailnet:       record.Tailnet,
	}, nil
}

func (s *Session) abandon(ctx context.Context, held *state.Held, record *Record) error {
	s.Log.Error("create failed; removing what it made", "workspace", record.Name, "machine", record.Machine)
	var left error
	if s.Enroller != nil {
		if err := s.Enroller.Leave(ctx, held, runner{s, record.Machine}, record.Tailnet); err != nil {
			left = fmt.Errorf("removing the tailnet node of %s after the failed create also failed, so delete it from the tailnet by hand: %w", record.Name, err)
		}
	}
	if err := errors.Join(left, s.discard(ctx, record.Machine)); err != nil {
		return fmt.Errorf("%w; the record of %s is kept so that destroy can retry", err, record.Name)
	}
	return s.forget(record.Name)
}

func (s *Session) discard(ctx context.Context, machine string) error {
	_, err := s.Provider.Get(ctx, machine)
	switch {
	case errors.Is(err, providers.ErrNotFound):
	case err != nil:
		return fmt.Errorf("could not tell whether %s is still there, so check the provider for it: %w", machine, err)
	default:
		if err := s.Provider.Destroy(ctx, machine); err != nil {
			return fmt.Errorf("removing %s failed, so check the provider for it: %w", machine, err)
		}
	}
	return s.Pool.Retire(machine, s.Now())
}

func (s *Session) Resume(ctx context.Context, name string) (*Result, error) {
	held, err := s.State.Hold(name)
	if err != nil {
		return nil, err
	}
	defer held.Release()
	record, err := s.record(name)
	if err != nil {
		return nil, err
	}
	if err := s.verifyTailnet(ctx); err != nil {
		return nil, err
	}
	if err := s.Pool.Start(record.Machine, s.Now()); err != nil {
		return nil, err
	}
	return s.restore(ctx, held, record)
}

func (s *Session) restore(ctx context.Context, held *state.Held, record *Record) (*Result, error) {
	if err := s.reopen(ctx, record); err != nil {
		return nil, err
	}
	if s.Enroller == nil {
		return s.deliver(ctx, record)
	}
	machine := runner{s, record.Machine}
	node, joined, err := s.Enroller.Reattach(ctx, held, machine, record.Tailnet)
	record.Tailnet = node
	if err != nil {
		return nil, err
	}
	result, err := s.deliver(ctx, record)
	if err != nil && joined && record.Tailnet != nil {
		return nil, errors.Join(err, s.Enroller.Leave(context.WithoutCancel(ctx), held, machine, record.Tailnet))
	}
	return result, err
}

func (s *Session) deliver(ctx context.Context, record *Record) (*Result, error) {
	result, err := s.connect(ctx, record)
	if err != nil {
		return nil, err
	}
	return result, s.save(record)
}

func (s *Session) reopen(ctx context.Context, record *Record) error {
	machine := record.Machine
	if err := s.Provider.Wake(ctx, machine); err != nil {
		return err
	}
	env, err := s.forwards(record)
	if err != nil {
		return err
	}
	if _, err := s.run(ctx, machine, RefreshScript(s.ProjectRoot(), env, s.profile.Prepare), nil); err != nil {
		return err
	}
	return s.bootstrap(ctx, machine, env, "")
}

func (s *Session) Suspend(ctx context.Context, name string) error {
	held, err := s.State.Hold(name)
	if err != nil {
		return err
	}
	defer held.Release()
	record, err := s.record(name)
	if err != nil {
		return err
	}
	if err := s.Provider.Suspend(ctx, record.Machine); err != nil {
		return err
	}
	return s.Pool.Stop(record.Machine, s.Now())
}

func (s *Session) Destroy(ctx context.Context, name string) error {
	held, err := s.State.Hold(name)
	if err != nil {
		return err
	}
	defer held.Release()
	record, err := s.record(name)
	if errors.Is(err, errNotRecorded) {
		record, err = s.orphanedClaim(name, err)
	}
	if errors.Is(err, errNotRecorded) {
		return s.forgetBinding(ctx, held, err)
	}
	if err != nil {
		return err
	}
	if s.Enroller != nil {
		if err := s.Enroller.Leave(ctx, held, runner{s, record.Machine}, record.Tailnet); err != nil {
			return err
		}
	}
	if err := s.Provider.Destroy(ctx, record.Machine); err != nil && !errors.Is(err, providers.ErrNotFound) {
		return err
	}
	if err := s.Pool.Retire(record.Machine, s.Now()); err != nil {
		return err
	}
	return s.forget(name)
}

func (s *Session) forgetBinding(ctx context.Context, held *state.Held, missing error) error {
	if s.Enroller == nil {
		return missing
	}
	binding, err := s.Enroller.Bindings.Read(held.Name)
	if err != nil {
		return err
	}
	if binding == (tailnet.Binding{}) {
		return missing
	}
	s.Log.Info("revoking the tailnet node an earlier attempt left bound to this name", "workspace", held.Name, "bound", binding)
	return s.Enroller.Forget(ctx, held)
}

func (s *Session) orphanedClaim(name string, missing error) (*Record, error) {
	machine, resource, err := s.Pool.Ledger.ClaimedFor(name)
	if err != nil {
		return nil, err
	}
	if machine == "" {
		return nil, missing
	}
	if resource.Provider != s.Kind || resource.Profile != s.Profile {
		return nil, fmt.Errorf("%s holds a claim on %s, a %s/%s spare, not %s/%s", name, machine, resource.Provider, resource.Profile, s.Kind, s.Profile)
	}
	s.Log.Info("destroying the spare a create claimed for this name but never recorded", "workspace", name, "machine", machine)
	return &Record{Name: name, Provider: s.Kind, Profile: s.Profile, Machine: machine, Claimed: true}, nil
}

func (s *Session) Prepare(ctx context.Context) error {
	if err := s.drainWhere(ctx, false); err != nil {
		return err
	}
	for {
		name := s.Pool.SpareName()
		reserved, err := s.Pool.Reserve(name, os.Getpid(), s.Now())
		if err != nil || !reserved {
			return err
		}
		s.Log.Info("preparing a spare", "machine", name, "provider", s.Kind, "profile", s.Profile)
		if _, err := s.Provider.Create(ctx, s.spec(name, map[string]string{LabelSpare: s.Pool.Fingerprint})); err != nil {
			return errors.Join(err, s.Pool.Retire(name, s.Now()))
		}
		s.Log.Info("created", "machine", name)
		if err := s.prepareSpare(ctx, name); err != nil {
			s.Log.Error("preparing the spare failed; removing it", "machine", name)
			return errors.Join(err, s.discard(context.WithoutCancel(ctx), name))
		}
		if err := s.Pool.MarkReady(name, s.Now()); err != nil {
			return err
		}
		s.Log.Info("spare ready", "machine", name)
	}
}

func (s *Session) prepareSpare(ctx context.Context, name string) error {
	if err := s.install(ctx, &Record{Machine: name, Source: Source{Ref: s.Config.Ref}}); err != nil {
		return err
	}
	if err := s.warm(ctx, name); err != nil {
		return err
	}
	clean := identity.CleanScript(s.ProjectRoot(), s.Config.Repository, s.Platform.CredentialHelper, s.Config.Identity.ForbiddenPaths)
	if _, err := s.run(ctx, name, clean, nil); err != nil {
		return err
	}
	return s.Provider.Suspend(ctx, name)
}

func (s *Session) replaceClaimed() {
	if s.Pool.Spares == 0 || s.Refill == nil {
		return
	}
	if err := s.Refill(); err != nil {
		s.Log.Error("could not start refilling the pool", "err", err)
	}
}

func (s *Session) Drain(ctx context.Context, all bool) error {
	return s.drainWhere(ctx, all)
}

func (s *Session) drainWhere(ctx context.Context, all bool) error {
	drained, err := s.Pool.Drain(all)
	if err != nil {
		return err
	}
	for _, name := range drained {
		s.Log.Info("destroying a spare", "machine", name)
		if err := s.discard(ctx, name); err != nil {
			return err
		}
	}
	return nil
}
