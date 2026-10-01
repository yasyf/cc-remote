package namespace

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	Name       = "namespace"
	DefaultCLI = "devbox"
	nameLimit  = 63
	readyPoll  = 5 * time.Second
)

type Config struct {
	CLI               string
	SSHDir            string
	StateDir          string
	Platform          string
	Image             string
	VolumeSizeGB      int
	IdleTimeout       time.Duration
	Site              string
	Sizes             map[string]string
	HourlyUSD         map[string]float64
	StorageGBMonthUSD float64
	CallTimeout       time.Duration
	ReadyTimeout      time.Duration
}

type Provider struct {
	Config
	Runner providers.Runner
}

var _ providers.Provider = (*Provider)(nil)

func New(config Config) *Provider {
	return &Provider{Config: config, Runner: providers.OSRunner{}}
}

func (p *Provider) records() providers.Records {
	return providers.Records{Dir: filepath.Join(p.StateDir, Name, "machines")}
}

func (p *Provider) devbox(ctx context.Context, args ...string) ([]byte, error) {
	if p.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.CallTimeout)
		defer cancel()
	}
	return p.devboxUnbounded(ctx, args...)
}

func (p *Provider) devboxUnbounded(ctx context.Context, args ...string) ([]byte, error) {
	return providers.Output(ctx, p.Runner, providers.Command{Name: p.CLI, Args: args})
}

func (p *Provider) Check(ctx context.Context) error {
	_, err := p.devbox(ctx, "auth", "check-login")
	var failed *providers.CommandError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("devbox CLI %q not found; install it from https://namespace.so/docs/devbox", p.CLI)
	case errors.As(err, &failed):
		return fmt.Errorf("not logged in to Namespace; run 'devbox login': %w", err)
	}
	return err
}

func (p *Provider) size(spec providers.Spec) (string, error) {
	if spec.Size != "" {
		return spec.Size, nil
	}
	size, ok := p.Sizes[spec.Profile]
	if !ok {
		return "", fmt.Errorf("namespace has no size for profile %q and the spec names none", spec.Profile)
	}
	return size, nil
}

func (p *Provider) Rate(spec providers.Spec) (providers.Rate, error) {
	size, err := p.size(spec)
	if err != nil {
		return providers.Rate{}, err
	}
	hourly, ok := p.HourlyUSD[size]
	if !ok {
		return providers.Rate{}, fmt.Errorf("namespace has no hourly rate for size %q", size)
	}
	return providers.Rate{HourlyUSD: hourly, StorageGB: float64(p.VolumeSizeGB), StorageGBMonthUSD: p.StorageGBMonthUSD}, nil
}

func (p *Provider) Create(ctx context.Context, spec providers.Spec) (providers.Machine, error) {
	if err := providers.CheckName(spec.Name, nameLimit); err != nil {
		return providers.Machine{}, err
	}
	size, err := p.size(spec)
	if err != nil {
		return providers.Machine{}, err
	}
	image := cmp.Or(spec.Image, p.Image)
	if image == "" {
		return providers.Machine{}, errors.New("namespace needs an image; set one in the namespace config or the spec")
	}
	switch _, err := p.Get(ctx, spec.Name); {
	case err == nil:
		return providers.Machine{}, fmt.Errorf("devbox %s: %w", spec.Name, providers.ErrExists)
	case !errors.Is(err, providers.ErrNotFound):
		return providers.Machine{}, err
	}
	args := []string{"create",
		"--name", spec.Name,
		"--platform", p.Platform,
		"--size", size,
		imageFlag(image), image,
		"--volume_size_gb", fmt.Sprint(p.VolumeSizeGB),
		"--auto_stop_idle_timeout", p.IdleTimeout.String(),
		"--no_checkout",
		"--persistent",
	}
	if site := cmp.Or(spec.Region, p.Site); site != "" {
		args = append(args, "--site", site)
	}
	if _, err := p.devboxUnbounded(ctx, args...); err != nil {
		return providers.Machine{}, err
	}
	if _, err := p.devbox(ctx, "configure-ssh", spec.Name); err != nil {
		return providers.Machine{}, err
	}
	if err := p.records().Save(spec.Name, spec.Labels); err != nil {
		return providers.Machine{}, err
	}
	return p.Get(ctx, spec.Name)
}

func imageFlag(image string) string {
	if strings.Contains(image, "/") {
		return "--image_ref"
	}
	return "--image"
}

type devbox struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	MainUser  string    `json:"main_user"`
}

const noDevboxes = "No devbox available yet"

func (p *Provider) list(ctx context.Context) ([]devbox, error) {
	out, err := p.devbox(ctx, "list", "-o", "json")
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(out)
	if bytes.HasPrefix(trimmed, []byte(noDevboxes)) {
		return nil, nil
	}
	var boxes []devbox
	if err := json.NewDecoder(bytes.NewReader(trimmed)).Decode(&boxes); err != nil {
		return nil, fmt.Errorf("reading the devbox list: %w", err)
	}
	return boxes, nil
}

func (p *Provider) find(ctx context.Context, id string) (devbox, error) {
	boxes, err := p.list(ctx)
	if err != nil {
		return devbox{}, err
	}
	for _, box := range boxes {
		if box.Name == id {
			return box, nil
		}
	}
	return devbox{}, fmt.Errorf("devbox %s: %w", id, providers.ErrNotFound)
}

func (p *Provider) machine(box devbox) (providers.Machine, error) {
	labels, err := p.records().Labels(box.Name)
	if err != nil {
		return providers.Machine{}, err
	}
	return providers.Machine{ID: box.Name, Provider: Name, State: providers.StateUnknown, CreatedAt: box.CreatedAt, Labels: labels}, nil
}

func (p *Provider) Get(ctx context.Context, id string) (providers.Machine, error) {
	box, err := p.find(ctx, id)
	if err != nil {
		return providers.Machine{}, err
	}
	return p.machine(box)
}

func (p *Provider) List(ctx context.Context, labels map[string]string) ([]providers.Machine, error) {
	boxes, err := p.list(ctx)
	if err != nil {
		return nil, err
	}
	var machines []providers.Machine
	for _, box := range boxes {
		machine, err := p.machine(box)
		if err != nil {
			return nil, err
		}
		if machine.HasLabels(labels) {
			machines = append(machines, machine)
		}
	}
	return machines, nil
}

func (p *Provider) Wake(ctx context.Context, id string) error {
	target, err := p.SSHTarget(ctx, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, p.ReadyTimeout)
	defer cancel()
	for {
		result, err := p.ssh(ctx, target, []string{"true"}, nil)
		if err != nil {
			return fmt.Errorf("waking devbox %s: %w", id, err)
		}
		if result.ExitCode == 0 {
			return nil
		}
		if permanent(result.Stderr) {
			return fmt.Errorf("devbox %s cannot start: %s", id, bytes.TrimSpace(result.Stderr))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("devbox %s not ready after %s: %s", id, p.ReadyTimeout, bytes.TrimSpace(result.Stderr))
		case <-time.After(readyPoll):
		}
	}
}

func permanent(stderr []byte) bool {
	for line := range strings.Lines(string(stderr)) {
		_, rest, ok := strings.Cut(line, "rpc error: code = ")
		if !ok {
			continue
		}
		code, _, _ := strings.Cut(rest, " ")
		return code == "FailedPrecondition"
	}
	return false
}

func (p *Provider) Suspend(ctx context.Context, id string) error {
	if _, err := p.find(ctx, id); err != nil {
		return err
	}
	_, err := p.devbox(ctx, "shutdown", id, "--force")
	return err
}

func (p *Provider) Destroy(ctx context.Context, id string) error {
	if _, err := p.find(ctx, id); err != nil {
		return err
	}
	if _, err := p.devbox(ctx, "expire", id, "--force"); err != nil {
		return err
	}
	return p.records().Remove(id)
}

func (p *Provider) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (providers.Result, error) {
	target, err := p.SSHTarget(ctx, id)
	if err != nil {
		return providers.Result{}, err
	}
	result, err := p.ssh(ctx, target, cmd, stdin)
	if err != nil {
		return providers.Result{}, err
	}
	if result.ExitCode == sshFailed {
		return providers.Result{}, &providers.CommandError{Command: "ssh " + target.Host, Result: result}
	}
	return result, nil
}

const sshFailed = 255

func (p *Provider) ssh(ctx context.Context, target providers.Target, cmd []string, stdin io.Reader) (providers.Result, error) {
	args := []string{"-T"}
	for _, option := range append(target.SSHOptions(),
		"BatchMode=yes",
		"LogLevel=ERROR",
		"ConnectTimeout=120",
		"ServerAliveInterval=15",
		"ServerAliveCountMax=4",
	) {
		args = append(args, "-o", option)
	}
	args = append(args, target.Host, "--", providers.ShellQuote(cmd...))
	return p.Runner.Run(ctx, providers.Command{Name: "ssh", Args: args, Stdin: stdin})
}
