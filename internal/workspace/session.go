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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/tailnet"
)

const (
	LabelWorkspace = "cc-remote/workspace"
	LabelProfile   = "cc-remote/profile"
	tailnetTimeout = 60 * time.Second
)

type Session struct {
	Config   *config.Config
	Provider providers.Provider
	Kind     string
	Profile  string
	Platform Platform
	State    state.Dir
	Enroller *tailnet.Enroller
	Log      *slog.Logger
	Stderr   io.Writer
	Token    func(context.Context) (string, error)
	Now      func() time.Time
	Scripts  images.Scripts
	Stamp    string

	profile        config.Profile
	labels         []LabelledEnv
	image          string
	imageSpec      string
	payload        *config.Payload
	privatePlugins bool
	stderr         sync.Mutex
}

func (s *Session) inPlace() bool {
	return s.image == ""
}

func Open(cfg *config.Config, provider providers.Provider, kind, profile string, platform Platform) (*Session, error) {
	spec, err := cfg.ProfileNamed(profile)
	if err != nil {
		return nil, err
	}
	machine := spec.Machine[kind]
	err = provider.ValidateSpec(providers.Spec{Name: "workspace", Profile: profile, Image: machine.Image, Size: machine.Size, Region: machine.Region})
	if err != nil {
		return nil, err
	}
	if supervisor := provider.Traits().Supervisor; machine.Payload != nil && supervisor != providers.SupervisorSpriteEnv {
		return nil, fmt.Errorf("profile %s: machine %s mounts a payload, which only a %s host remounts at boot; provider %s runs %s", profile, kind, providers.SupervisorSpriteEnv, kind, supervisor)
	}
	rendered, err := render(cfg, profile, machine.Image != "")
	if err != nil {
		return nil, err
	}
	if err := coverEnv(cfg.Forwards, rendered.scripts.Env); err != nil {
		return nil, err
	}
	s := &Session{
		Config:         cfg,
		Provider:       provider,
		Kind:           kind,
		Profile:        profile,
		Platform:       platform,
		State:          cfg.State(),
		Log:            slog.Default(),
		Stderr:         os.Stderr,
		Now:            time.Now,
		Scripts:        rendered.scripts,
		Stamp:          rendered.stamp,
		profile:        spec,
		image:          machine.Image,
		imageSpec:      rendered.imageSpec,
		payload:        machine.Payload,
		privatePlugins: rendered.private,
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

type rendered struct {
	scripts   images.Scripts
	stamp     string
	imageSpec string
	private   bool
}

func render(cfg *config.Config, profile string, imaged bool) (rendered, error) {
	inventory, err := images.Load(cfg.ScriptPath(cfg.Inventory))
	if err != nil {
		return rendered{}, err
	}
	scripts, err := images.Render(inventory, profile)
	if err != nil {
		return rendered{}, err
	}
	private := slices.ContainsFunc(inventory.Claude.Marketplaces, func(m images.Marketplace) bool { return m.Private })
	if !imaged {
		return rendered{scripts: scripts, stamp: images.Stamp(scripts, nil), private: private}, nil
	}
	image, err := images.RenderImage(inventory)
	if err != nil {
		return rendered{}, err
	}
	return rendered{scripts: scripts, stamp: images.Stamp(scripts, &image), imageSpec: image.Fingerprint(), private: private}, nil
}

func coverEnv(forwards []config.Forward, declared []string) error {
	names := make([]string, 0, len(forwards))
	for _, forward := range forwards {
		names = append(names, forward.Env)
	}
	slices.Sort(names)
	if want := slices.Sorted(slices.Values(declared)); !slices.Equal(names, want) {
		return fmt.Errorf("forwards export %v but the inventory's configure.env declares %v; the two must name the same variables", names, want)
	}
	return nil
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
	return s.execute(ctx, machine, []string{"sh", "-c", script}, stdin)
}

func (s *Session) execute(ctx context.Context, machine string, argv []string, stdin io.Reader) ([]byte, error) {
	result, err := s.Provider.Exec(ctx, machine, argv, stdin)
	if err != nil {
		return nil, err
	}
	if err := s.writeStderr(result.Stderr); err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return result.Stdout, fmt.Errorf("%s on %s exited %d: %s", argv[0], machine, result.ExitCode, bytes.TrimSpace(result.Stderr))
	}
	return result.Stdout, nil
}

func (s *Session) writeStderr(output []byte) error {
	s.stderr.Lock()
	defer s.stderr.Unlock()
	_, err := s.Stderr.Write(output)
	return err
}

func (s *Session) exec(machine string) images.Exec {
	return func(ctx context.Context, argv []string, stdin io.Reader) error {
		_, err := s.execute(ctx, machine, argv, stdin)
		return err
	}
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
	return errors.Join(state.Remove(s.State.Workspace(name)), state.Remove(s.State.SSH(name)))
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
	if err := s.unbound(name); err != nil {
		return nil, err
	}
	now := s.Now()
	record := &Record{Name: name, Provider: s.Kind, Profile: s.Profile, Source: source, Machine: name, Image: s.image, ImageSpec: s.imageSpec, Unverified: true, CreatedAt: now}
	if err := s.save(record); err != nil {
		return nil, err
	}
	s.Log.Info("creating", "workspace", name, "provider", s.Kind, "profile", s.Profile)
	if _, err := s.Provider.Create(ctx, s.spec(name, map[string]string{LabelWorkspace: name})); err != nil {
		return nil, s.unmade(record, err)
	}
	record.Unverified = false
	if err := s.save(record); err != nil {
		return nil, errors.Join(err, s.abandon(context.WithoutCancel(ctx), held, record))
	}
	s.Log.Info("created", "machine", name)
	result, err := s.provision(ctx, held, record)
	if err == nil {
		err = s.save(record)
	}
	if err != nil {
		return nil, errors.Join(err, s.abandon(context.WithoutCancel(ctx), held, record))
	}
	return result, nil
}

func (s *Session) unmade(record *Record, cause error) error {
	if errors.Is(cause, providers.ErrExists) {
		return errors.Join(cause, s.forget(record.Name))
	}
	return fmt.Errorf("%w; whether %s came to exist at the provider is unverified, so its record is kept: run destroy %s once the provider answers", cause, record.Machine, record.Name)
}

func (s *Session) reconcile(name string, recorded *Record) error {
	if recorded == nil {
		return nil
	}
	return fmt.Errorf("%s already exists and is already recorded under %s as machine %s; resume it or destroy it before creating again", name, s.State, recorded.Machine)
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
	env, err := s.forwards(record)
	if err != nil {
		return nil, err
	}
	run := newLanes(ctx)
	run.Go(func(ctx context.Context) error { return s.installPackages(ctx, machine) })
	run.Go(func(ctx context.Context) error {
		if err := s.checkout(ctx, record); err != nil {
			return err
		}
		return s.prepare(ctx, record, env)
	})
	run.Go(func(ctx context.Context) error {
		if err := s.installPlugins(ctx, machine); err != nil {
			return err
		}
		if err := s.configure(ctx, record, env); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return s.enroll(context.WithoutCancel(ctx), held, record)
	})
	if err := run.Wait(); err != nil {
		return nil, err
	}
	if err := s.publish(ctx, machine); err != nil {
		return nil, err
	}
	return s.connect(ctx, record)
}

func (s *Session) installTools(ctx context.Context, machine string) error {
	run := newLanes(ctx)
	run.Go(func(ctx context.Context) error { return s.installPackages(ctx, machine) })
	run.Go(func(ctx context.Context) error { return s.installPlugins(ctx, machine) })
	if err := run.Wait(); err != nil {
		return err
	}
	return s.publish(ctx, machine)
}

func (s *Session) installPackages(ctx context.Context, machine string) error {
	if !s.inPlace() {
		return nil
	}
	if err := s.Scripts.Provision(ctx, s.exec(machine), images.PhasePackages); err != nil {
		return err
	}
	s.Log.Info("installed the packages", "machine", machine)
	return nil
}

func (s *Session) installPlugins(ctx context.Context, machine string) error {
	run := s.exec(machine)
	var payload string
	if s.payload != nil {
		if err := s.mountPayload(ctx, machine, run); err != nil {
			return err
		}
		payload = s.payload.SHA256
	}
	if s.inPlace() {
		if err := s.Scripts.Provision(ctx, run, images.PhaseTools); err != nil {
			return err
		}
		s.Log.Info("provisioned", "machine", machine)
	}
	var token string
	if s.privatePlugins {
		var err error
		if token, err = s.Token(ctx); err != nil {
			return err
		}
	}
	if err := s.Scripts.StagePlugins(ctx, run); err != nil {
		return err
	}
	if err := s.Scripts.Install(ctx, run, token, payload); err != nil {
		return err
	}
	s.Log.Info("installed the tools", "machine", machine, "stamp", s.Stamp[:12])
	return nil
}

func (s *Session) mountPayload(ctx context.Context, machine string, run images.Exec) (err error) {
	image, err := os.Open(s.Config.ScriptPath(s.payload.Path))
	if err != nil {
		return fmt.Errorf("open payload: %w", err)
	}
	defer func() { err = errors.Join(err, image.Close()) }()
	if err = s.Scripts.StagePayload(ctx, run, image, s.payload.SHA256); err != nil {
		return err
	}
	if err = s.Scripts.Provision(ctx, run, images.PhasePayload, s.payload.SHA256, s.Scripts.Fingerprint()); err != nil {
		return err
	}
	s.Log.Info("mounted the payload", "machine", machine, "sha256", s.payload.SHA256[:12])
	return nil
}

func (s *Session) publish(ctx context.Context, machine string) error {
	if err := s.Scripts.Publish(ctx, s.exec(machine), s.Stamp); err != nil {
		return err
	}
	s.Log.Info("published the tools", "machine", machine, "stamp", s.Stamp[:12])
	return nil
}

func (s *Session) readyTools(ctx context.Context, record *Record) error {
	machine := record.Machine
	if record.Image != s.image || record.ImageSpec != s.imageSpec {
		return fmt.Errorf("the image of profile %s changed since %s was created (image %q with declaration %.12s, now %q with %.12s); a machine cannot change its image in place, so destroy %s and create it again", s.Profile, record.Name, record.Image, record.ImageSpec, s.image, s.imageSpec, record.Name)
	}
	err := s.Scripts.Ready(ctx, s.exec(machine), s.Stamp)
	if err == nil {
		s.Log.Info("the tools are ready at the current stamp", "machine", machine, "stamp", s.Stamp[:12])
		return nil
	}
	s.Log.Info("the tools are not at the current stamp; provisioning them again", "machine", machine, "stamp", s.Stamp[:12], "err", err)
	return s.installTools(ctx, machine)
}

func (s *Session) checkout(ctx context.Context, record *Record) error {
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
	return nil
}

func (s *Session) prepare(ctx context.Context, record *Record, env map[string]string) error {
	if _, err := s.run(ctx, record.Machine, RefreshScript(s.ProjectRoot(), ExportEnv(env), s.profile.Prepare), nil); err != nil {
		return err
	}
	s.Log.Info("prepared", "machine", record.Machine)
	return nil
}

func (s *Session) configure(ctx context.Context, record *Record, env map[string]string) error {
	if err := s.Scripts.Configure(ctx, s.exec(record.Machine), env); err != nil {
		return err
	}
	s.Log.Info("configured", "machine", record.Machine)
	return nil
}

func (s *Session) checkoutToken(ctx context.Context) (string, error) {
	if !TokenAllowed(s.Config.Repository) {
		return "", nil
	}
	return s.Token(ctx)
}

func (s *Session) forwards(record *Record) (map[string]string, error) {
	forwards, err := AllocateForwards(s.labels, record.Forwards)
	if err != nil {
		return nil, err
	}
	record.Forwards = forwards
	return ForwardEnv(forwards, s.labels), nil
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
	fragment := s.State.SSH(record.Name)
	if err := state.Write(fragment, SSHFragment(record.Name, target)); err != nil {
		return nil, err
	}
	ssh := sshFromTarget(target)
	ssh.Config = fragment
	return &Result{
		SchemaVersion: SchemaVersion,
		Name:          record.Name,
		Provider:      s.Kind,
		Profile:       s.Profile,
		Source:        record.Source,
		Machine:       record.Machine,
		ProjectRoot:   s.ProjectRoot(),
		SSH:           ssh,
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
	if err := errors.Join(left, s.discard(ctx, record.Machine, record.Name)); err != nil {
		return fmt.Errorf("%w; the record of %s is kept so that destroy can retry", err, record.Name)
	}
	return s.forget(record.Name)
}

func (s *Session) discard(ctx context.Context, machine, owner string) error {
	found, err := s.Provider.Get(ctx, machine)
	switch {
	case errors.Is(err, providers.ErrNotFound):
	case err != nil:
		return fmt.Errorf("could not tell whether %s is still there, so check the provider for it: %w", machine, err)
	default:
		if found.Labels[LabelWorkspace] != owner {
			return fmt.Errorf("%s exists at the provider without a label proving this workspace state made it (want %s=%s, labels %v, created %s), so it was left running and its record is kept: if it is yours, remove it at the provider, then run this again", machine, LabelWorkspace, owner, found.Labels, found.CreatedAt.Format(time.RFC3339))
		}
		if err := s.Provider.Destroy(ctx, machine); err != nil {
			return fmt.Errorf("removing %s failed, so check the provider for it: %w", machine, err)
		}
		if _, err := s.Provider.Get(ctx, machine); !errors.Is(err, providers.ErrNotFound) {
			return fmt.Errorf("%s is still at the provider after its destroy, so check the provider for it: %w", machine, err)
		}
	}
	return nil
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
	if record.Unverified {
		return nil, fmt.Errorf("the create of %s never confirmed that %s came to exist, so it was not provisioned: run destroy %s, then create it again", record.Name, record.Machine, record.Name)
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
	if err := s.readyTools(ctx, record); err != nil {
		return err
	}
	env, err := s.forwards(record)
	if err != nil {
		return err
	}
	if err := s.prepare(ctx, record, env); err != nil {
		return err
	}
	return s.configure(ctx, record, env)
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
	return nil
}

func (s *Session) Destroy(ctx context.Context, name string) error {
	held, err := s.State.Hold(name)
	if err != nil {
		return err
	}
	defer held.Release()
	record, err := s.record(name)
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
	if err := s.discard(ctx, record.Machine, name); err != nil {
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
