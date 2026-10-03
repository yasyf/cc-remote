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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/tailnet"
)

const (
	LabelWorkspace      = "cc-remote/workspace"
	LabelProfile        = "cc-remote/profile"
	tailnetTimeout      = 60 * time.Second
	urlCommandTimeout   = 60 * time.Second
	urlCommandWaitDelay = 2 * time.Second
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

	profile          config.Profile
	labels           []LabelledEnv
	image            string
	imageSpec        string
	payload          *config.Payload
	closure          bool
	privatePlugins   bool
	tailnetFromTools bool
	stderr           sync.Mutex
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
	rendered, err := render(cfg, profile, machine, machine.Payload != nil)
	if err != nil {
		return nil, err
	}
	if machine.Payload != nil && !rendered.closure {
		return nil, fmt.Errorf("profile %s: machine %s mounts a payload, which needs apt.payload in the inventory; only a closure payload is admitted and mounted", profile, kind)
	}
	if machine.Payload != nil && machine.Payload.Packages == nil {
		return nil, fmt.Errorf("profile %s: machine %s mounts a payload whose inventory declares apt.payload, so it installs the resident packages from the archive its payload build wrote; pin it under payload.packages", profile, kind)
	}
	if err := coverEnv(cfg.Forwards, rendered.scripts.Env); err != nil {
		return nil, err
	}
	s := &Session{
		Config:           cfg,
		Provider:         provider,
		Kind:             kind,
		Profile:          profile,
		Platform:         platform,
		State:            cfg.State(),
		Log:              slog.Default(),
		Stderr:           os.Stderr,
		Now:              time.Now,
		Scripts:          rendered.scripts,
		Stamp:            rendered.stamp,
		profile:          spec,
		image:            machine.Image,
		imageSpec:        rendered.imageSpec,
		payload:          machine.Payload,
		closure:          rendered.closure,
		privatePlugins:   rendered.private,
		tailnetFromTools: rendered.tailnetFromTools,
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
	scripts          images.Scripts
	stamp            string
	imageSpec        string
	private          bool
	tailnetFromTools bool
	closure          bool
}

func render(cfg *config.Config, profile string, machine config.Machine, payload bool) (rendered, error) {
	inventory, err := images.Load(cfg.ScriptPath(cfg.Inventory))
	if err != nil {
		return rendered{}, err
	}
	if !payload {
		inventory.Apt.Payload = nil
	}
	scripts, err := images.Render(inventory, profile)
	if err != nil {
		return rendered{}, err
	}
	out := rendered{
		scripts:          scripts,
		private:          slices.ContainsFunc(inventory.Claude.Marketplaces, func(m images.Marketplace) bool { return m.Private }),
		tailnetFromTools: inventory.TailnetFromTools(),
		closure:          inventory.Apt.Payload != nil,
	}
	if machine.Image == "" {
		var payload, packages string
		if machine.Payload != nil {
			payload = machine.Payload.SHA256
		}
		if machine.Payload != nil && machine.Payload.Packages != nil {
			packages = machine.Payload.Packages.SHA256
		}
		out.stamp = images.Stamp(scripts, nil, payload, packages)
		return out, nil
	}
	image, err := images.RenderImage(inventory)
	if err != nil {
		return rendered{}, err
	}
	out.stamp, out.imageSpec = images.Stamp(scripts, &image, "", ""), image.Fingerprint()
	return out, nil
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
	countExec(ctx)
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

func (s *Session) capture(machine string) images.Capture {
	return func(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
		countExec(ctx)
		result, err := s.Provider.Exec(ctx, machine, argv, stdin)
		if err != nil {
			return nil, err
		}
		if result.ExitCode != 0 {
			return nil, fmt.Errorf("%s on %s exited %d", argv[0], machine, result.ExitCode)
		}
		return result.Stdout, nil
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

func (s *Session) Create(ctx context.Context, name string, source Source) (result *Result, err error) {
	ctx = s.begin(ctx)
	defer func() { s.summarize(ctx, name, err) }()
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
	staged, err := s.sources(ctx, name)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	record := &Record{Name: name, Provider: s.Kind, Profile: s.Profile, Source: source, Machine: name, Image: s.image, ImageSpec: s.imageSpec, Unverified: true, CreatedAt: now}
	if err := s.save(record); err != nil {
		return nil, err
	}
	s.Log.Info("creating", "workspace", name, "provider", s.Kind, "profile", s.Profile)
	if err := s.timed(ctx, laneMain, "machine.create", name, func() error {
		_, err := s.Provider.Create(ctx, s.spec(name, map[string]string{LabelWorkspace: name}))
		return err
	}); err != nil {
		return nil, s.unmade(record, err)
	}
	record.Unverified = false
	if err := s.save(record); err != nil {
		return nil, errors.Join(err, s.abandon(context.WithoutCancel(ctx), held, record))
	}
	s.Log.Info("created", "machine", name)
	result, err = s.provision(ctx, held, record, staged)
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

func (s *Session) provision(ctx context.Context, held *state.Held, record *Record, staged transfers) (*Result, error) {
	machine := record.Machine
	env, err := s.forwards(record)
	if err != nil {
		return nil, err
	}
	if err := s.installPrerequisites(ctx, machine); err != nil {
		return nil, err
	}
	run := newLanes(ctx)
	packages, tools := s.installLanes(run, machine, staged)
	loader := run.Go(func(ctx context.Context) error {
		if err := run.after(packages, tools); err != nil {
			return err
		}
		return s.registerClosure(ctx, machine)
	})
	run.Go(func(ctx context.Context) error {
		if err := s.checkout(ctx, record); err != nil {
			return err
		}
		if len(s.profile.Prepare) == 0 {
			return nil
		}
		if err := run.after(loader); err != nil {
			return err
		}
		return s.prepare(ctx, record, env)
	})
	run.Go(func(ctx context.Context) error {
		if err := run.after(loader); err != nil {
			return err
		}
		return s.configure(ctx, record, env)
	})
	enrollAfter := []<-chan struct{}{packages, tools}
	if s.tailnetFromTools {
		enrollAfter = []<-chan struct{}{tools}
	}
	run.Go(func(ctx context.Context) error {
		if err := run.after(enrollAfter...); err != nil {
			return err
		}
		return s.enroll(context.WithoutCancel(ctx), held, record)
	})
	if err := run.Wait(); err != nil {
		return nil, err
	}
	tail := newLanes(ctx)
	var target providers.Target
	tail.Go(func(ctx context.Context) error { return s.publish(ctx, machine) })
	tail.Go(func(ctx context.Context) (err error) {
		target, err = s.sshTarget(ctx, machine)
		return err
	})
	if err := tail.Wait(); err != nil {
		return nil, err
	}
	return s.deliverTarget(record, target)
}

func (s *Session) installTools(ctx context.Context, machine string) error {
	staged, err := s.sources(ctx, machine)
	if err != nil {
		return err
	}
	if err := s.installPrerequisites(ctx, machine); err != nil {
		return err
	}
	run := newLanes(ctx)
	s.installLanes(run, machine, staged)
	if err := run.Wait(); err != nil {
		return err
	}
	if err := s.registerClosure(ctx, machine); err != nil {
		return err
	}
	return s.publish(ctx, machine)
}

func (s *Session) installLanes(run *lanes, machine string, staged transfers) (packages, tools <-chan struct{}) {
	mounted := run.Go(func(ctx context.Context) error { return s.mountPayload(ctx, machine, staged.payload) })
	packages = run.Go(func(ctx context.Context) error { return s.installPackages(ctx, machine, staged.packages) })
	tools = run.Go(func(ctx context.Context) error {
		if err := run.after(mounted); err != nil {
			return err
		}
		return s.installPlugins(ctx, machine)
	})
	return packages, tools
}

func (s *Session) installPrerequisites(ctx context.Context, machine string) error {
	if !s.inPlace() {
		return nil
	}
	return s.timed(ctx, laneMain, "prerequisites", machine, func() error {
		return s.Scripts.Provision(ctx, s.exec(machine), images.PhasePrerequisites)
	})
}

func (s *Session) installPackages(ctx context.Context, machine string, stage transfer) error {
	if !s.inPlace() {
		return nil
	}
	var mode []string
	switch {
	case s.closure:
		if err := stage(ctx, machine, s.exec(machine)); err != nil {
			return err
		}
		mode = []string{images.PackagesResident, s.payload.Packages.SHA256}
	case s.payload != nil:
		mode = []string{images.PackagesResident}
	}
	if err := s.timed(ctx, lanePackages, "packages", machine, func() error {
		return s.Scripts.Provision(ctx, s.exec(machine), images.PhasePackages, mode...)
	}); err != nil {
		return err
	}
	s.Log.Info("installed the packages", "machine", machine)
	return nil
}

func (s *Session) registerClosure(ctx context.Context, machine string) error {
	if !s.inPlace() || s.payload == nil || !s.closure {
		return nil
	}
	if err := s.timed(ctx, laneLoader, "loader", machine, func() error {
		return s.Scripts.Provision(ctx, s.exec(machine), images.PhaseLoader)
	}); err != nil {
		return err
	}
	s.Log.Info("registered the payload's closure with the loader", "machine", machine)
	return nil
}

func (s *Session) installPlugins(ctx context.Context, machine string) error {
	run := s.exec(machine)
	var payload string
	if s.payload != nil {
		payload = s.payload.SHA256
	}
	if s.inPlace() {
		if err := s.timed(ctx, laneTools, "tools.provision", machine, func() error {
			return s.Scripts.Provision(ctx, run, images.PhaseTools)
		}); err != nil {
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
	if err := s.timed(ctx, laneTools, "plugins.stage", machine, func() error {
		return s.Scripts.StagePlugins(ctx, run, payload)
	}); err != nil {
		return err
	}
	if err := s.timed(ctx, laneTools, "tools.install", machine, func() error {
		return s.Scripts.Install(ctx, run, token, payload)
	}); err != nil {
		return err
	}
	s.Log.Info("installed the tools", "machine", machine, "stamp", s.Stamp[:12])
	return nil
}

type transfer func(ctx context.Context, machine string, run images.Exec) error

type transfers struct {
	payload  transfer
	packages transfer
}

type artifactSource struct {
	artifact images.TransferArtifact
	lane     string
	source   config.Source
}

func (s *Session) sources(ctx context.Context, machine string) (transfers, error) {
	if s.payload == nil {
		return transfers{}, nil
	}
	var staged transfers
	var err error
	if staged.payload, err = s.source(ctx, machine, artifactSource{images.PayloadArtifact, laneMount, s.payload.Source}); err != nil {
		return transfers{}, err
	}
	if s.payload.Packages == nil {
		return staged, nil
	}
	if staged.packages, err = s.source(ctx, machine, artifactSource{images.PackagesArtifact, lanePackages, *s.payload.Packages}); err != nil {
		return transfers{}, err
	}
	return staged, nil
}

func (s *Session) source(ctx context.Context, machine string, from artifactSource) (transfer, error) {
	if from.source.Path != "" {
		return func(ctx context.Context, machine string, run images.Exec) error {
			return s.stream(ctx, machine, run, from)
		}, nil
	}
	var url images.PayloadURL
	if err := s.timed(ctx, laneMain, from.artifact.Label+".url", machine, func() error {
		var err error
		url, err = s.sourceURL(ctx, from)
		return err
	}); err != nil {
		return nil, err
	}
	return func(ctx context.Context, machine string, run images.Exec) error {
		if err := s.timed(ctx, from.lane, from.artifact.Label+".fetch", machine, func() error {
			return s.Scripts.Fetch(ctx, run, from.artifact, url, from.source.SHA256, from.source.Size)
		}); err != nil {
			return err
		}
		s.Log.Info("fetched the "+from.artifact.Name, "sha256", from.source.SHA256[:12])
		return nil
	}, nil
}

func (s *Session) sourceURL(ctx context.Context, from artifactSource) (images.PayloadURL, error) {
	ctx, cancel := context.WithTimeout(ctx, urlCommandTimeout)
	defer cancel()
	command := from.source.URLCommand
	cmd := exec.CommandContext(ctx, s.Config.ScriptPath(command[0]), slices.Concat(command[1:], []string{from.source.SHA256, strconv.FormatInt(from.source.Size, 10)})...)
	cmd.Dir = filepath.Dir(s.Config.Path)
	cmd.WaitDelay = urlCommandWaitDelay
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return images.PayloadURL{}, fmt.Errorf("%s url_command %s: %w", from.artifact.Name, command[0], err)
	}
	line, _, more := strings.Cut(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if more {
		return images.PayloadURL{}, fmt.Errorf("%s url_command %s printed more than one line", from.artifact.Name, command[0])
	}
	url, err := images.ParsePayloadURL(line)
	if err != nil {
		return images.PayloadURL{}, fmt.Errorf("%s url_command %s: %w", from.artifact.Name, command[0], err)
	}
	return url, nil
}

func (s *Session) mountPayload(ctx context.Context, machine string, stage transfer) error {
	if s.payload == nil {
		return nil
	}
	run := s.exec(machine)
	if err := stage(ctx, machine, run); err != nil {
		return err
	}
	args := []string{s.payload.SHA256, s.Scripts.Fingerprint()}
	if s.closure {
		args = append(args, s.payload.Packages.SHA256)
	}
	if err := s.timed(ctx, laneMount, "payload.mount", machine, func() error {
		return s.Scripts.Provision(ctx, run, images.PhasePayload, args...)
	}); err != nil {
		return err
	}
	s.Log.Info("mounted the payload", "machine", machine, "sha256", s.payload.SHA256[:12])
	return nil
}

func (s *Session) stream(ctx context.Context, machine string, run images.Exec, from artifactSource) (err error) {
	name := from.artifact.Name
	// O_NONBLOCK keeps a FIFO from blocking the open until a writer appears, so the fstat can reject it.
	file, err := os.OpenFile(s.Config.ScriptPath(from.source.Path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("open %s: %s is not a regular file (mode %v)", name, file.Name(), info.Mode())
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("open %s: %s is readable by others (mode %v); only its owner may read a %s", name, file.Name(), info.Mode(), name)
	}
	if owner := info.Sys().(*syscall.Stat_t).Uid; int(owner) != os.Getuid() {
		return fmt.Errorf("open %s: %s is owned by uid %d, not by this user (uid %d)", name, file.Name(), owner, os.Getuid())
	}
	return s.timed(ctx, from.lane, from.artifact.Label+".stage", machine, func() error {
		return s.Scripts.Stage(ctx, run, from.artifact, file, from.source.SHA256)
	})
}

func (s *Session) publish(ctx context.Context, machine string) error {
	if err := s.timed(ctx, lanePublish, "tools.publish", machine, func() error {
		return s.Scripts.Publish(ctx, s.exec(machine), s.Stamp)
	}); err != nil {
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
	if err := s.timed(ctx, laneCheckout, "checkout", machine, func() error {
		_, err := s.run(ctx, machine, CheckoutScript(root, s.Config.Repository, record.Source, s.profile.Checkout == config.Shallow), strings.NewReader(token+"\n"))
		return err
	}); err != nil {
		return err
	}
	s.Log.Info("checked out", "machine", machine, "ref", record.Source.Ref, "head", record.Source.Head)
	return nil
}

func (s *Session) prepare(ctx context.Context, record *Record, env map[string]string) error {
	if err := s.timed(ctx, laneCheckout, "prepare", record.Machine, func() error {
		_, err := s.run(ctx, record.Machine, RefreshScript(s.ProjectRoot(), ExportEnv(env), s.profile.Prepare), nil)
		return err
	}); err != nil {
		return err
	}
	s.Log.Info("prepared", "machine", record.Machine)
	return nil
}

func (s *Session) configure(ctx context.Context, record *Record, env map[string]string) error {
	if err := s.timed(ctx, laneConfigure, "configure", record.Machine, func() error {
		return s.Scripts.Configure(ctx, s.exec(record.Machine), env)
	}); err != nil {
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
	return s.timed(ctx, laneEnroll, "tailnet.enroll", record.Machine, func() error {
		node, err := s.Enroller.Enroll(ctx, held, runner{s, record.Machine})
		record.Tailnet = node
		return err
	})
}

func (s *Session) connect(ctx context.Context, record *Record) (*Result, error) {
	target, err := s.sshTarget(ctx, record.Machine)
	if err != nil {
		return nil, err
	}
	return s.deliverTarget(record, target)
}

func (s *Session) sshTarget(ctx context.Context, machine string) (providers.Target, error) {
	var target providers.Target
	err := s.timed(ctx, laneSSH, "ssh.target", machine, func() error {
		var err error
		target, err = s.Provider.SSHTarget(ctx, machine)
		return err
	})
	return target, err
}

func (s *Session) deliverTarget(record *Record, target providers.Target) (*Result, error) {
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
	ctx = s.begin(ctx)
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
